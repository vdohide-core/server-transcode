package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"server-transcode/internal/config"
	"server-transcode/internal/db/models"
	"server-transcode/internal/transcoder"
	"server-transcode/internal/uploader"
	"server-transcode/internal/utils"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func newUUID() string { return uuid.New().String() }

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ─── Find and Claim ───────────────────────────────────────────

func findAndClaimFile(ctx context.Context) (*models.VideoProcess, *models.File, error) {
	// Pre-load fileIds that have permanent transcode errors — exclude from query
	var excludeFileIDs []string
	errorDocs, _ := models.FileErrorModel.Find(ctx, bson.M{"errorType": "transcode"})
	for _, e := range errorDocs {
		excludeFileIDs = append(excludeFileIDs, e.FileID)
	}

	// Find files that are ready, video type, NOT cloned, NOT trashed/deleted, NOT in file_errors
	filter := bson.M{
		"status":             models.FileStatusReady,
		"type":               models.FileTypeVideo,
		"clonedFrom":         bson.M{"$exists": false},
		"metadata.trashedAt": bson.M{"$exists": false},
		"metadata.deletedAt": bson.M{"$exists": false},
	}
	if len(excludeFileIDs) > 0 {
		filter["_id"] = bson.M{"$nin": excludeFileIDs}
	}

	sortOrder := transcodeSortOrder
	if len(sortOrder) == 0 {
		sortOrder = bson.D{{Key: "metadata.size", Value: 1}, {Key: "createdAt", Value: 1}}
	}
	opts := options.Find().SetSort(sortOrder).SetLimit(20)

	cursor, err := models.FileModel.FindRaw(ctx, filter, opts)
	if err != nil {
		return nil, nil, err
	}
	defer cursor.Close(ctx)

	candidateCount := 0
	skipReasons := map[string]int{}

	for cursor.Next(ctx) {
		var file models.File
		if err := cursor.Decode(&file); err != nil {
			continue
		}
		candidateCount++

		// Must have original video media
		originalCount, _ := models.MediaModel.CountDocuments(ctx, bson.M{
			"fileId":     file.ID,
			"type":       models.MediaTypeVideo,
			"resolution": models.ResolutionOriginal,
		})
		if originalCount == 0 {
			skipReasons["no_original"]++
			continue
		}

		// Check if already has transcoded resolutions (any of 360/480/720/1080)
		transcodedCount, _ := models.MediaModel.CountDocuments(ctx, bson.M{
			"fileId":     file.ID,
			"type":       models.MediaTypeVideo,
			"resolution": bson.M{"$in": []string{models.Resolution360, models.Resolution480, models.Resolution720, models.Resolution1080}},
		})
		if transcodedCount > 0 {
			skipReasons["already_transcoded"]++
			continue
		}

		// Check no active transcode process (including permanently_failed)
		activeCount, _ := models.VideoProcessModel.CountDocuments(ctx, bson.M{
			"fileId":      file.ID,
			"processType": models.ProcessTypeTranscode,
			"status": bson.M{"$in": []string{
				models.ProcessStatusProcessing,
				models.ProcessStatusFailed,
				"permanently_failed",
			}},
		})
		if activeCount > 0 {
			skipReasons["active_process"]++
			continue
		}

		// Try to claim this file
		process, err := claimFile(ctx, &file)
		if err != nil {
			log.Printf("⚠️  [%s] Claim failed: %v", file.Slug, err)
			skipReasons["claim_failed"]++
			continue
		}
		return process, &file, nil
	}

	if candidateCount > 0 {
		log.Printf("⏭️  Checked %d candidates, all skipped: %v", candidateCount, skipReasons)
	}

	return nil, nil, nil
}

func claimFile(ctx context.Context, file *models.File) (*models.VideoProcess, error) {
	now := time.Now()
	processing := models.ProcessStatusProcessing
	pending := models.StepStatusPending

	process := &models.VideoProcess{
		ID:          newUUID(),
		FileID:      &file.ID,
		Slug:        &file.Slug,
		WorkerID:    &workerID,
		Status:      &processing,
		SpaceID:     file.SpaceID,
		ProcessType: models.ProcessTypeTranscode,
		Timeline: bson.M{
			"download": bson.M{"status": pending},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if _, err := models.VideoProcessModel.Create(ctx, process); err != nil {
		log.Printf("⚠️  [%s] claimFile: Create FAILED: %v", file.Slug, err)
		return nil, err
	}
	log.Printf("🆕 [%s] claimFile: Created process %s (fileId=%s)", file.Slug, process.ID, file.ID)

	return process, nil
}

// ─── Storage Resolution ───────────────────────────────────────

func resolveStorage(ctx context.Context, sourceStorageID string) (*models.Storage, error) {
	localPath := config.AppConfig.StoragePath
	localID := config.AppConfig.StorageId

	if localPath != "" && localID != "" {
		dbStorage, err := models.StorageModel.FindByID(ctx, localID)
		if err == nil {
			if !dbStorage.Enable || dbStorage.Status != models.StorageStatusOnline {
				log.Println("⛔ Local storage disabled/offline — looking for alternative...")
				return findAlternativeStorage(ctx, localID)
			}
		}
		return &models.Storage{ID: localID}, nil
	}

	// Remote mode: prefer same storage as original file
	if sourceStorageID != "" {
		srcStorage, err := models.StorageModel.FindByID(ctx, sourceStorageID)
		if err == nil && srcStorage.IsOnline() && srcStorage.HasSSHCredentials() {
			log.Printf("📦 Using same storage as original: %s", srcStorage.Name)
			return srcStorage, nil
		}
	}

	// Find best storage with SSH credentials
	return findAlternativeStorage(ctx, "")
}

func findAlternativeStorage(ctx context.Context, excludeID string) (*models.Storage, error) {
	filter := bson.M{
		"enable":             true,
		"status":             models.StorageStatusOnline,
		"type":               models.StorageTypeLocal,
		"local.ssh.username": bson.M{"$exists": true, "$ne": ""},
		"local.ssh.password": bson.M{"$exists": true, "$ne": ""},
		"local.ssh.port":     bson.M{"$gt": 0},
	}
	if excludeID != "" {
		filter["_id"] = bson.M{"$ne": excludeID}
	}

	storage, err := models.StorageModel.FindOne(ctx, filter,
		options.FindOne().SetSort(bson.M{"capacity.percentage": 1}))
	if err != nil {
		return nil, fmt.Errorf("no storage with SSH credentials available")
	}

	log.Printf("📦 Using alternative storage: %s", storage.Name)
	return storage, nil
}

// ─── Main Transcode Process ──────────────────────────────────

func runTranscode(ctx context.Context, process *models.VideoProcess) error {
	fileID := derefStr(process.FileID)
	slug := derefStr(process.Slug)

	// Start per-job logger
	procLogger := utils.NewProcessLogger(slug)
	defer procLogger.Close()

	exePath, _ := os.Executable()
	baseDir := filepath.Dir(exePath)
	if strings.Contains(exePath, "go-build") {
		baseDir, _ = os.Getwd()
	}
	downloadDir := filepath.Join(baseDir, "download", slug)
	os.MkdirAll(downloadDir, 0755)

	// Track success — only cleanup on success
	var transcodeSuccess bool
	defer func() {
		if transcodeSuccess {
			os.RemoveAll(downloadDir)
			log.Printf("🧹 [%s] Cleaned up temp dir", slug)
		} else {
			log.Printf("⚠️  [%s] Keeping temp dir for retry: %s", slug, downloadDir)
		}
	}()

	utils.LogMain("🎬 [%s] START TRANSCODE", slug)

	// ─── Find original media and its storage ─────────────────────
	originalMedia, err := findOriginalMedia(ctx, fileID)
	if err != nil {
		failProcess(ctx, process.ID, slug, "original media not found")
		return fmt.Errorf("original media not found: %w", err)
	}

	sourceStorageID := derefStr(originalMedia.StorageID)
	sourceStorage, err := models.StorageModel.FindByID(ctx, sourceStorageID)
	if err != nil {
		failProcess(ctx, process.ID, slug, fmt.Sprintf("source storage not found: %s", sourceStorageID))
		return fmt.Errorf("source storage not found: %w", err)
	}

	// ─── STEP 1: DOWNLOAD original from storage ──────────────────
	originalPath := filepath.Join(downloadDir, models.FileNameOriginal)

	// Skip download if file already exists AND size matches media record
	if info, statErr := os.Stat(originalPath); statErr == nil && info.Size() > 0 {
		// Validate file size against media record to detect incomplete downloads
		expectedSize := int64(0)
		if originalMedia.Metadata != nil {
			switch v := originalMedia.Metadata.Size.(type) {
			case int64:
				expectedSize = v
			case float64:
				expectedSize = int64(v)
			case int:
				expectedSize = int64(v)
			case int32:
				expectedSize = int64(v)
			}
		}

		if expectedSize > 0 && info.Size() != expectedSize {
			log.Printf("⚠️  [%s] Original file size mismatch (local=%.2f MB, expected=%.2f MB) — re-downloading",
				slug, float64(info.Size())/1024/1024, float64(expectedSize)/1024/1024)
			os.Remove(originalPath)
		} else {
			log.Printf("📥 [%s] Original already cached (%.2f MB) — skipping download", slug, float64(info.Size())/1024/1024)
			completeStep(ctx, process.ID, "download")
			updateOverallPercent(ctx, process.ID, 10)
		}
	}

	if _, statErr := os.Stat(originalPath); statErr != nil {
		startStep(ctx, process.ID, "download")

		localStoragePath := config.AppConfig.StoragePath
		localStorageID := config.AppConfig.StorageId

		if localStoragePath != "" && localStorageID != "" && sourceStorageID == localStorageID {
			// Local copy — file is on this machine
			originalFileName := derefStr(originalMedia.FileName)
			srcPath := filepath.Join(localStoragePath, fileID, originalFileName)
			log.Printf("📥 [%s] Copying original from local: %s", slug, srcPath)

			srcFile, err := os.Open(srcPath)
			if err != nil {
				failProcess(ctx, process.ID, slug, fmt.Sprintf("local file not found: %v", err))
				return err
			}
			dstFile, err := os.Create(originalPath)
			if err != nil {
				srcFile.Close()
				failProcess(ctx, process.ID, slug, fmt.Sprintf("create temp file: %v", err))
				return err
			}

			srcInfo, _ := srcFile.Stat()
			totalSize := srcInfo.Size()
			buf := make([]byte, 1024*1024)
			var copied int64
			var lastReported int64

			for {
				n, readErr := srcFile.Read(buf)
				if n > 0 {
					dstFile.Write(buf[:n])
					copied += int64(n)
					if copied-lastReported >= 5*1024*1024 || copied == totalSize {
						lastReported = copied
						percent := float64(copied) / float64(totalSize) * 100
						updateTimelineStep(ctx, process.ID, "download", models.StepStatusProcessing, percent)
						updateOverallPercent(ctx, process.ID, percent*0.1) // download = 10% of overall
					}
				}
				if readErr != nil {
					break
				}
			}
			srcFile.Close()
			dstFile.Close()
		} else if sourceStorage.HasSSHCredentials() {
			// Download via SCP (Node.js script)
			log.Printf("📥 [%s] Downloading original via SCP from %s", slug, sourceStorage.Name)

			originalFileName := derefStr(originalMedia.FileName)
			remotePath := sourceStorage.GetPath() + "/" + fileID + "/" + originalFileName
			scpConfig := uploader.SCPDownloadConfig{
				Host:       sourceStorage.GetHost(),
				Port:       sourceStorage.Local.SSH.Port,
				Username:   sourceStorage.Local.SSH.Username,
				Password:   sourceStorage.Local.SSH.Password,
				RemotePath: remotePath,
				LocalPath:  originalPath,
			}
			if scpConfig.Port == 0 {
				scpConfig.Port = 22
			}

			err = uploader.DownloadViaSCP(scpConfig, func(p uploader.SCPProgress) {
				if p.Type == "progress" && p.Total > 0 {
					percent := float64(p.Transferred) / float64(p.Total) * 100
					updateTimelineStep(ctx, process.ID, "download", models.StepStatusProcessing, percent)
					updateOverallPercent(ctx, process.ID, percent*0.1)
				}
			})
			if err != nil {
				failProcess(ctx, process.ID, slug, fmt.Sprintf("SCP download failed: %v", err))
				return err
			}
		} else {
			failProcess(ctx, process.ID, slug, "no download method available for source storage")
			return fmt.Errorf("no download method")
		}

		completeStep(ctx, process.ID, "download")
		updateOverallPercent(ctx, process.ID, 10)
		log.Printf("✅ [%s] Download complete", slug)
	}

	if isCancelled(ctx, process.ID) {
		return nil
	}

	// ─── STEP 2: PROBE video info ────────────────────────────────
	log.Printf("🔍 [%s] Probing video info...", slug)
	videoInfo, err := transcoder.ProbeVideoInfo(originalPath)
	if err != nil {
		failProcess(ctx, process.ID, slug, fmt.Sprintf("probe failed: %v", err))
		return err
	}

	orientation := transcoder.DetectOrientation(videoInfo.Width, videoInfo.Height)
	shortSide := transcoder.ShortSide(videoInfo.Width, videoInfo.Height)
	log.Printf("📐 [%s] Video: %dx%d, duration: %ds, codec: %s, orientation: %s, shortSide: %d, bitrate: %dkbps",
		slug, videoInfo.Width, videoInfo.Height, videoInfo.Duration, videoInfo.Codec, orientation, shortSide, videoInfo.VideoBitrate)

	// ─── STEP 3: DETERMINE resolutions ───────────────────────────
	targetResolutions := transcoder.DetermineResolutions(shortSide)

	// Filter out resolutions that already have media records
	var pendingResolutions []string
	for _, res := range targetResolutions {
		resPtr := res
		existingCount, _ := models.MediaModel.CountDocuments(ctx, bson.M{
			"fileId":     fileID,
			"type":       models.MediaTypeVideo,
			"resolution": resPtr,
		})
		if existingCount == 0 {
			pendingResolutions = append(pendingResolutions, res)
		} else {
			log.Printf("⏭️  [%s] Skip %sp — already exists", slug, res)
		}
	}

	if len(pendingResolutions) == 0 {
		log.Printf("✅ [%s] All resolutions already exist — nothing to transcode", slug)
		log.Printf("🗑️  [%s] DELETE process %s — reason: pendingResolutions=0", slug, process.ID)
		models.VideoProcessModel.DeleteByID(ctx, process.ID)
		transcodeSuccess = true
		return nil
	}

	// Update process with target resolutions
	models.VideoProcessModel.UpdateByID(ctx, process.ID, bson.M{"$set": bson.M{
		"resolutions": pendingResolutions,
		"updatedAt":   time.Now(),
	}})

	log.Printf("🎯 [%s] Target resolutions: %v", slug, pendingResolutions)

	// Initialize timeline for each resolution (encode + upload) + thumbnail
	timelineInit := bson.M{}
	for _, res := range pendingResolutions {
		encodeKey := fmt.Sprintf("timeline.encode_%s", res)
		uploadKey := fmt.Sprintf("timeline.upload_%s", res)
		timelineInit[encodeKey+".status"] = models.StepStatusPending
		timelineInit[uploadKey+".status"] = models.StepStatusPending
	}
	timelineInit["timeline.thumbnail.status"] = models.StepStatusPending
	timelineInit["updatedAt"] = time.Now()
	models.VideoProcessModel.UpdateByID(ctx, process.ID, bson.M{"$set": timelineInit})

	// Acquire processing lock (CPU only — GPU can run multiple encodes simultaneously)
	encoder := transcoder.DetectEncoder()
	var procLock *utils.ProcessingLock
	if encoder == transcoder.EncoderCPU {
		log.Printf("🔒 [%s] Waiting for processing lock (CPU mode)...", slug)
		procLock = utils.AcquireProcessingLock("processing")
		defer procLock.Release()
		log.Printf("🔒 [%s] Processing lock acquired", slug)
	} else {
		log.Printf("🎮 [%s] GPU mode — skipping processing lock", slug)
	}

	// Resolve upload storage
	uploadStorage, err := resolveStorage(ctx, sourceStorageID)
	if err != nil {
		failProcess(ctx, process.ID, slug, fmt.Sprintf("no upload storage: %v", err))
		return err
	}

	// ─── STEP 4: ENCODE + UPLOAD + CREATE MEDIA (sequential) ────
	// Progress allocation: download=10%, encode=70%, thumbnail=10%, final=10%
	encodeProgressBase := 10.0
	encodeProgressTotal := 70.0
	perResProgress := encodeProgressTotal / float64(len(pendingResolutions))

	var highestResolution int

	for idx, res := range pendingResolutions {
		if isCancelled(ctx, process.ID) {
			return nil
		}

		stepName := fmt.Sprintf("encode_%s", res)
		shortSideTarget := models.ResolutionToShortSide[res]
		fileName := models.ResolutionToFileName[res]
		outputPath := filepath.Join(downloadDir, fileName)

		targetW, targetH := transcoder.GetScaleDimensions(videoInfo.Width, videoInfo.Height, shortSideTarget)

		needEncode := true

		// Skip only if BOTH encoded file AND media record exist (fully completed)
		if outInfo, statErr := os.Stat(outputPath); statErr == nil && outInfo.Size() > 0 {
			existingMedia, _ := models.MediaModel.CountDocuments(ctx, bson.M{
				"fileId": fileID, "type": models.MediaTypeVideo, "resolution": res,
			})
			if existingMedia > 0 {
				log.Printf("⏭️  [%s] %sp already encoded+uploaded — skipping", slug, res)
				completeStep(ctx, process.ID, stepName)
				resInt, _ := strconv.Atoi(res)
				if resInt > highestResolution {
					highestResolution = resInt
				}
				addCompleted(ctx, process.ID, res)
				continue
			}
			// File exists but no media record — may be corrupt from killed process
			log.Printf("⚠️  [%s] %sp file exists but no media record — re-encoding", slug, res)
			os.Remove(outputPath)
		}

		if needEncode {
			startStep(ctx, process.ID, stepName)

			log.Printf("🎬 [%s] Encoding %sp (%dx%d)...", slug, res, targetW, targetH)

			err := transcoder.EncodeResolution(originalPath, outputPath, targetW, targetH, videoInfo.DurationF, videoInfo.VideoBitrate, int(shortSide), func(percent int) {
				updateTimelineStep(ctx, process.ID, stepName, models.StepStatusProcessing, float64(percent))
				overallPercent := encodeProgressBase + float64(idx)*perResProgress + float64(percent)/100.0*perResProgress
				updateOverallPercent(ctx, process.ID, overallPercent)
			})
			if err != nil {
				failProcess(ctx, process.ID, slug, fmt.Sprintf("encode %sp failed: %v", res, err))
				return err
			}

			completeStep(ctx, process.ID, stepName)
			log.Printf("✅ [%s] Encoded %sp", slug, res)
		}

		// Upload this resolution
		uploadStepName := fmt.Sprintf("upload_%s", res)
		startStep(ctx, process.ID, uploadStepName)
		log.Printf("📤 [%s] Uploading %s...", slug, fileName)
		onUploadProgress := func(p uploader.SCPProgress) {
			if p.Type == "progress" && p.Total > 0 {
				percent := float64(p.Transferred) / float64(p.Total) * 100
				updateTimelineStep(ctx, process.ID, uploadStepName, models.StepStatusProcessing, percent)
			}
		}
		if err := uploadFile(ctx, process, uploadStorage, outputPath, fileName, onUploadProgress); err != nil {
			// If permission/auth error, try alternative storage before failing
			if isPermissionError(err) && uploadStorage.ID != "" {
				log.Printf("⚠️  [%s] Permission denied on %s — trying alternative storage...", slug, uploadStorage.Name)
				altStorage, altErr := findAlternativeStorage(ctx, uploadStorage.ID)
				if altErr == nil {
					log.Printf("📦 [%s] Retrying upload on %s", slug, altStorage.Name)
					if retryErr := uploadFile(ctx, process, altStorage, outputPath, fileName, onUploadProgress); retryErr == nil {
						uploadStorage = altStorage // use alt storage for remaining uploads
						goto uploadSuccess
					} else {
						log.Printf("⚠️  [%s] Alternative storage also failed: %v", slug, retryErr)
					}
				}
			}
			failProcess(ctx, process.ID, slug, fmt.Sprintf("upload %s failed: %v", fileName, err))
			return err
		}
	uploadSuccess:
		completeStep(ctx, process.ID, uploadStepName)

		// Create media record
		fileSize := transcoder.GetFileSize(outputPath)
		resInt, _ := strconv.Atoi(res)

		now := time.Now()
		resPtr := res
		fnPtr := fileName
		storageIDPtr := uploadStorage.ID
		mediaSlug := utils.RandomString(11, false)

		media := models.Media{
			ID:         newUUID(),
			Type:       models.MediaTypeVideo,
			FileName:   &fnPtr,
			Resolution: &resPtr,
			StorageID:  &storageIDPtr,
			Slug:       mediaSlug,
			FileID:     &fileID,
			Metadata: &models.MediaMetadata{
				Size:     fileSize,
				Width:    int(targetW),
				Height:   int(targetH),
				Duration: float64(videoInfo.Duration),
			},
			CreatedAt: now,
			UpdatedAt: now,
		}
		models.MediaModel.Create(ctx, &media)
		log.Printf("✅ [%s] Created media record for %sp", slug, res)

		// Clone media to files that were cloned from this original
		cloneMediaToClonedFiles(ctx, fileID, media, slug)

		addCompleted(ctx, process.ID, res)

		// Track highest resolution
		if resInt > highestResolution {
			highestResolution = resInt
		}

		// Delete temp file — keep 360p for thumbnail, delete others immediately
		if res != models.Resolution360 {
			os.Remove(outputPath)
			log.Printf("🗑️  [%s] Removed temp %s", slug, fileName)
		}
	}

	// ─── STEP 5: THUMBNAIL — sprite sheet + sprite.vtt ───────────
	if isCancelled(ctx, process.ID) {
		return nil
	}

	startStep(ctx, process.ID, "thumbnail")
	updateOverallPercent(ctx, process.ID, 80)

	// Try file_360.mp4 first (smallest = fastest), fallback to original
	thumbInput := filepath.Join(downloadDir, models.ResolutionToFileName[models.Resolution360])
	if _, err := os.Stat(thumbInput); os.IsNotExist(err) {
		thumbInput = originalPath
		log.Printf("📌 [%s] file_360.mp4 not found, using original for thumbnails", slug)
	}

	log.Printf("🖼️  [%s] Generating sprite thumbnails from %s...", slug, filepath.Base(thumbInput))
	spriteResult, err := transcoder.GenerateSpriteThumbnails(thumbInput, downloadDir, videoInfo.DurationF)
	if err != nil {
		log.Printf("⚠️  [%s] Sprite generation failed: %v — skipping thumbnails", slug, err)
	} else {
		// Clean up any temp files before uploading
		os.Remove(filepath.Join(spriteResult.SpriteDir, "cropped_last.jpg"))

		// Upload entire sprite/ folder
		log.Printf("📤 [%s] Uploading sprite folder...", slug)
		if err := uploadDir(ctx, process, uploadStorage, spriteResult.SpriteDir, "sprite"); err != nil {
			log.Printf("⚠️  [%s] Failed to upload sprite folder: %v", slug, err)
		}

		// Calculate total sprite size
		var totalSpriteSize int64
		for _, spriteFileName := range spriteResult.SpriteFiles {
			totalSpriteSize += transcoder.GetFileSize(filepath.Join(spriteResult.SpriteDir, spriteFileName))
		}

		// Create thumbnail media record
		now := time.Now()
		thumbFn := "sprite.vtt"
		storageIDPtr := uploadStorage.ID

		thumbMedia := models.Media{
			ID:        newUUID(),
			Type:      models.MediaTypeThumbnail,
			FileName:  &thumbFn,
			StorageID: &storageIDPtr,
			Slug:      utils.RandomString(11, false),
			FileID:    &fileID,
			Metadata: &models.MediaMetadata{
				Size: totalSpriteSize,
			},
			CreatedAt: now,
			UpdatedAt: now,
		}
		models.MediaModel.Create(ctx, &thumbMedia)
		log.Printf("✅ [%s] Created thumbnail media record", slug)

		// Clone thumbnail media to cloned files
		cloneMediaToClonedFiles(ctx, fileID, thumbMedia, slug)
	}

	completeStep(ctx, process.ID, "thumbnail")
	updateOverallPercent(ctx, process.ID, 90)

	// Delete 360p temp file (kept for thumbnail)
	file360Path := filepath.Join(downloadDir, models.ResolutionToFileName[models.Resolution360])
	if _, err := os.Stat(file360Path); err == nil {
		os.Remove(file360Path)
		log.Printf("🗑️  [%s] Removed temp file_360.mp4 (after thumbnails)", slug)
	}

	// ─── STEP 6: UPDATE FILE metadata.highest ────────────────────
	if highestResolution > 0 {
		models.FileModel.UpdateByID(ctx, fileID, bson.M{"$set": bson.M{
			"metadata.highest": highestResolution,
			"updatedAt":        time.Now(),
		}})
		log.Printf("✅ [%s] Updated file.metadata.highest = %d", slug, highestResolution)

		// Update metadata.highest for cloned files too
		updateClonedFilesHighest(ctx, fileID, highestResolution, slug)
	}

	// ─── STEP 7: CLEANUP ─────────────────────────────────────────
	updateOverallPercent(ctx, process.ID, 100)

	// ─── STEP 8: CLOUDFLARE CACHE PURGE ──────────────────────────
	purgePlaylistCache(ctx, slug, fileID)

	// Mark success so deferred cleanup runs
	transcodeSuccess = true

	// Delete process record (success)
	log.Printf("🗑️  [%s] DELETE process %s — reason: transcode success", slug, process.ID)
	models.VideoProcessModel.DeleteByID(ctx, process.ID)

	log.Println(strings.Repeat("=", 50))
	utils.LogMain("✅ [%s] Transcode complete! Resolutions: %v", slug, pendingResolutions)
	log.Println(strings.Repeat("=", 50))

	return nil
}

// ─── Upload Helpers ──────────────────────────────────────────

func uploadFile(_ context.Context, process *models.VideoProcess, storage *models.Storage, localPath, fileName string, onProgress uploader.OnSCPProgress) error {
	fileID := derefStr(process.FileID)
	localStoragePath := config.AppConfig.StoragePath
	localStorageID := config.AppConfig.StorageId

	if localStoragePath != "" && localStorageID != "" {
		// Local storage — move file
		_, err := uploader.MoveFileLocal(localStoragePath, fileID, localPath, fileName, nil)
		return err
	}

	if storage.HasSSHCredentials() {
		// SCP via Node.js script
		remotePath := storage.GetPath()
		scpConfig := uploader.SCPConfig{
			LocalPath:  localPath,
			Host:       storage.GetHost(),
			Port:       storage.Local.SSH.Port,
			Username:   storage.Local.SSH.Username,
			Password:   storage.Local.SSH.Password,
			RemotePath: fmt.Sprintf("%s/%s", remotePath, fileID),
			FileName:   fileName,
		}
		if scpConfig.Port == 0 {
			scpConfig.Port = 22
		}

		return uploader.UploadViaSCP(scpConfig, onProgress)
	}

	return fmt.Errorf("no upload method available for storage %s", storage.ID)
}

func uploadDir(_ context.Context, process *models.VideoProcess, storage *models.Storage, localDir, remoteDirName string) error {
	fileID := derefStr(process.FileID)
	localStoragePath := config.AppConfig.StoragePath
	localStorageID := config.AppConfig.StorageId

	if localStoragePath != "" && localStorageID != "" {
		// Local storage — copy directory
		destDir := filepath.Join(localStoragePath, fileID, remoteDirName)
		os.MkdirAll(destDir, 0755)

		entries, err := os.ReadDir(localDir)
		if err != nil {
			return fmt.Errorf("read dir: %w", err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			_, err := uploader.MoveFileLocal(localStoragePath, fileID, filepath.Join(localDir, entry.Name()), remoteDirName+"/"+entry.Name(), nil)
			if err != nil {
				return fmt.Errorf("copy %s: %w", entry.Name(), err)
			}
		}
		return nil
	}

	if storage.HasSSHCredentials() {
		remotePath := storage.GetPath()
		scpConfig := uploader.SCPDirConfig{
			LocalDir:   localDir,
			Host:       storage.GetHost(),
			Port:       storage.Local.SSH.Port,
			Username:   storage.Local.SSH.Username,
			Password:   storage.Local.SSH.Password,
			RemotePath: fmt.Sprintf("%s/%s/%s", remotePath, fileID, remoteDirName),
		}
		if scpConfig.Port == 0 {
			scpConfig.Port = 22
		}
		return uploader.UploadDirViaSCP(scpConfig)
	}

	return fmt.Errorf("no upload method available for storage %s", storage.ID)
}

// ─── Find Original Media ─────────────────────────────────────

func findOriginalMedia(ctx context.Context, fileID string) (*models.Media, error) {
	res := models.ResolutionOriginal
	media, err := models.MediaModel.FindOne(ctx, bson.M{
		"fileId":     fileID,
		"type":       models.MediaTypeVideo,
		"resolution": res,
	})
	if err != nil {
		return nil, err
	}
	return media, nil
}
