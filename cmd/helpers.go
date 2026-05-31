package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"server-transcode/internal/db/models"
	"server-transcode/internal/utils"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── Error Categorization ────────────────────────────────────

func categorizeError(errMsg string) string {
	e := strings.ToLower(errMsg)
	switch {
	case strings.Contains(e, "codec") || strings.Contains(e, "encode"):
		return "codec"
	case strings.Contains(e, "ffmpeg") || strings.Contains(e, "ffprobe"):
		return "ffmpeg"
	case strings.Contains(e, "upload") || strings.Contains(e, "ssh") || strings.Contains(e, "sftp") || strings.Contains(e, "scp"):
		return "upload"
	case strings.Contains(e, "download") || strings.Contains(e, "timeout") || strings.Contains(e, "connection"):
		return "network"
	case strings.Contains(e, "probe"):
		return "probe"
	case strings.Contains(e, "thumbnail") || strings.Contains(e, "sprite"):
		return "thumbnail"
	default:
		return "unknown"
	}
}

// ─── isCancelled ─────────────────────────────────────────────

func isCancelled(ctx context.Context, processID string) bool {
	p, err := models.VideoProcessModel.FindByID(ctx, processID)
	if err != nil {
		// Record missing ≠ cancelled — another service may have deleted it
		// Continue processing rather than silently aborting
		log.Printf("⚠️  isCancelled: FindByID(%s) error: %v — NOT treating as cancelled", processID, err)
		return false
	}
	status := derefStr(p.Status)
	if status == models.ProcessStatusCancelled {
		log.Printf("⚠️  isCancelled: process %s has status=cancelled", processID)
		return true
	}
	return false
}

// ─── failProcess ─────────────────────────────────────────────
// For transcode: does NOT change file status (file stays "ready").
// Marks process as failed. After 3 retries, keeps failed process as log.

func failProcess(ctx context.Context, processID, slug, errMsg string) {
	utils.LogMain("❌ [%s] ERROR: %s", slug, errMsg)
	category := categorizeError(errMsg)

	// Read current retryCount → increment manually
	retryNum := 1
	current, _ := models.VideoProcessModel.FindByID(ctx, processID)
	if current != nil && current.RetryCount != nil {
		retryNum = *current.RetryCount + 1
	}
	log.Printf("🔍 [%s] failProcess: processID=%s, currentRetry=%v, newRetry=%d", slug, processID, current != nil && current.RetryCount != nil, retryNum)

	// Use raw MongoDB UpdateOne to avoid goose auto-wrapping $set
	result, err := models.VideoProcessModel.Col().UpdateOne(ctx,
		bson.M{"_id": processID},
		bson.M{"$set": bson.M{
			"status":        models.ProcessStatusFailed,
			"error":         errMsg,
			"errorCategory": category,
			"retryCount":    retryNum,
			"updatedAt":     time.Now(),
		}},
	)

	if err != nil {
		log.Printf("❌ [%s] Process update failed: %v", slug, err)
		return
	}

	log.Printf("🔍 [%s] failProcess: matched=%d, modified=%d", slug, result.MatchedCount, result.ModifiedCount)

	// Verify readback
	verify, _ := models.VideoProcessModel.FindByID(ctx, processID)
	if verify != nil {
		vRetry := 0
		if verify.RetryCount != nil {
			vRetry = *verify.RetryCount
		}
		log.Printf("🔍 [%s] failProcess verify: status=%s, retryCount=%d", slug, derefStr(verify.Status), vRetry)
	} else {
		log.Printf("⚠️  [%s] failProcess verify: process NOT FOUND after update!", slug)
	}

	log.Printf("❌ [%s] Failed: %s [%s] (retry %d/3)", slug, errMsg, category, retryNum)

	// Diagnostic: delayed re-check — see if record disappears while we wait
	time.Sleep(2 * time.Second)
	recheck, _ := models.VideoProcessModel.FindByID(ctx, processID)
	if recheck == nil {
		log.Printf("🚨 [%s] CRITICAL: process %s VANISHED within 2 seconds of failProcess!", slug, processID)
		// Check if ANY documents exist
		allDocs, _ := models.VideoProcessModel.Find(ctx, bson.M{})
		log.Printf("🚨 [%s] Collection has %d documents after vanish", slug, len(allDocs))
	} else {
		log.Printf("✅ [%s] Process %s still exists after 2s delay", slug, processID)
	}
}

// ─── Progress Helpers ────────────────────────────────────────

func updateTimelineStep(ctx context.Context, processID, step, status string, percent float64) {
	models.VideoProcessModel.UpdateByID(ctx, processID, bson.M{"$set": bson.M{
		fmt.Sprintf("timeline.%s.status", step):  status,
		fmt.Sprintf("timeline.%s.percent", step): percent,
		"updatedAt":                              time.Now(),
	}})
}

func startStep(ctx context.Context, processID, step string) {
	now := time.Now()
	models.VideoProcessModel.UpdateByID(ctx, processID, bson.M{"$set": bson.M{
		fmt.Sprintf("timeline.%s.status", step):    models.StepStatusProcessing,
		fmt.Sprintf("timeline.%s.percent", step):   0,
		fmt.Sprintf("timeline.%s.startedAt", step): now,
		"updatedAt": now,
	}})
}

func completeStep(ctx context.Context, processID, step string) {
	now := time.Now()
	models.VideoProcessModel.UpdateByID(ctx, processID, bson.M{"$set": bson.M{
		fmt.Sprintf("timeline.%s.status", step):  models.StepStatusCompleted,
		fmt.Sprintf("timeline.%s.percent", step): 100,
		fmt.Sprintf("timeline.%s.endedAt", step): now,
		"updatedAt":                              now,
	}})
}

func updateOverallPercent(ctx context.Context, processID string, percent float64) {
	models.VideoProcessModel.UpdateByID(ctx, processID, bson.M{"$set": bson.M{
		"overallPercent": percent,
		"updatedAt":      time.Now(),
	}})
}

func addCompleted(ctx context.Context, processID, resolution string) {
	models.VideoProcessModel.UpdateByID(ctx, processID, bson.M{
		"$push": bson.M{"completed": resolution},
		"$set":  bson.M{"updatedAt": time.Now()},
	})
}

// ─── Clone media to cloned files ─────────────────────────────

func cloneMediaToClonedFiles(ctx context.Context, sourceFileID string, media models.Media, slug string) {
	cursor, err := models.FileModel.FindRaw(ctx, bson.M{
		"clonedFrom":         sourceFileID,
		"type":               models.FileTypeVideo,
		"metadata.trashedAt": bson.M{"$exists": false},
		"metadata.deletedAt": bson.M{"$exists": false},
	})
	if err != nil {
		return
	}
	defer cursor.Close(ctx)

	for cursor.Next(ctx) {
		var clonedFile models.File
		if err := cursor.Decode(&clonedFile); err != nil {
			continue
		}

		filter := bson.M{"fileId": clonedFile.ID, "type": media.Type}
		if media.Resolution != nil {
			filter["resolution"] = *media.Resolution
		}
		existCount, _ := models.MediaModel.CountDocuments(ctx, filter)
		if existCount > 0 {
			continue
		}

		now := time.Now()
		slug11 := utils.RandomString(11, true)
		clonedMedia := models.Media{
			ID:         uuid.New().String(),
			Type:       media.Type,
			FileName:   media.FileName,
			MimeType:   media.MimeType,
			Resolution: media.Resolution,
			StorageID:  media.StorageID,
			Slug:       slug11,
			FileID:     &clonedFile.ID,
			Metadata:   media.Metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		clonedFrom := sourceFileID
		clonedMedia.ClonedFrom = &clonedFrom

		if _, err := models.MediaModel.Create(ctx, &clonedMedia); err != nil {
			log.Printf("⚠️  [%s] Failed to clone media to %s: %v", slug, clonedFile.ID, err)
			continue
		}
		log.Printf("📋 [%s] Cloned media → file %s", slug, clonedFile.ID)
	}
}

// ─── Update cloned files highest ─────────────────────────────

func updateClonedFilesHighest(ctx context.Context, sourceFileID string, highest int, slug string) {
	result, err := models.FileModel.UpdateMany(ctx, bson.M{
		"clonedFrom":         sourceFileID,
		"type":               models.FileTypeVideo,
		"metadata.trashedAt": bson.M{"$exists": false},
		"metadata.deletedAt": bson.M{"$exists": false},
	}, bson.M{"$set": bson.M{
		"metadata.highest": highest,
		"updatedAt":        time.Now(),
	}})
	if err != nil {
		log.Printf("⚠️  [%s] Failed to update cloned files highest: %v", slug, err)
		return
	}
	if result != nil && result.ModifiedCount > 0 {
		log.Printf("📋 [%s] Updated metadata.highest=%d for %d cloned files", slug, highest, result.ModifiedCount)
	}
}

// ─── Cloudflare Cache Purge ──────────────────────────────────

func purgePlaylistCache(ctx context.Context, slug, fileID string) {
	// Read settings
	domainSetting, err := models.SettingModel.FindOne(ctx, bson.M{"name": models.SettingDomainContent})
	if err != nil {
		return // domain_content not configured — skip
	}
	domain := domainSetting.GetString("")
	if domain == "" {
		return
	}
	if !strings.HasPrefix(domain, "http://") && !strings.HasPrefix(domain, "https://") {
		domain = "https://" + domain
	}
	domain = strings.TrimRight(domain, "/")

	zoneSetting, err := models.SettingModel.FindOne(ctx, bson.M{"name": models.SettingCfZoneID})
	if err != nil {
		return
	}
	tokenSetting, err := models.SettingModel.FindOne(ctx, bson.M{"name": models.SettingCfApiToken})
	if err != nil {
		return
	}

	cfConfig := utils.CloudflareConfig{
		ZoneID:   zoneSetting.GetString(""),
		APIToken: tokenSetting.GetString(""),
	}
	if cfConfig.ZoneID == "" || cfConfig.APIToken == "" {
		return
	}

	// Collect URLs to purge: original file + all clones
	var purgeURLs []string
	purgeURLs = append(purgeURLs, fmt.Sprintf("%s/%s/playlist.m3u8", domain, slug))

	// Find cloned files' slugs
	cursor, err := models.FileModel.FindRaw(ctx, bson.M{
		"clonedFrom":         fileID,
		"type":               models.FileTypeVideo,
		"metadata.trashedAt": bson.M{"$exists": false},
		"metadata.deletedAt": bson.M{"$exists": false},
	}, options.Find().SetProjection(bson.M{"slug": 1}))
	if err == nil {
		defer cursor.Close(ctx)
		for cursor.Next(ctx) {
			var clonedFile models.File
			if err := cursor.Decode(&clonedFile); err != nil {
				continue
			}
			if clonedFile.Slug != "" {
				purgeURLs = append(purgeURLs, fmt.Sprintf("%s/%s/playlist.m3u8", domain, clonedFile.Slug))
			}
		}
	}

	log.Printf("☁️  [%s] Purging %d playlist URL(s) from Cloudflare cache...", slug, len(purgeURLs))
	for _, u := range purgeURLs {
		log.Printf("   → %s", u)
	}

	if err := utils.PurgeCloudflareCache(cfConfig, purgeURLs); err != nil {
		log.Printf("⚠️  [%s] Cloudflare purge failed: %v", slug, err)
	} else {
		log.Printf("✅ [%s] Cloudflare cache purged", slug)
	}
}

// isPermissionError checks if an error is related to SSH/SCP permission denied.
func isPermissionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "auth fail")
}
