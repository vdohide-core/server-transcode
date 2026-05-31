package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"server-transcode/internal/config"
	"server-transcode/internal/db/database"
	"server-transcode/internal/db/models"
	"server-transcode/internal/handlers"
	"server-transcode/internal/logger"
	"server-transcode/internal/middleware"
	"server-transcode/internal/transcoder"
	"server-transcode/internal/utils"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

var workerID string
var transcodeSortOrder bson.D

func main() {
	config.Load()
	workerID = utils.GenerateWorkerID()
	log.Printf("Starting Server Transcode [Worker: %s]", workerID)

	// Init file logger (writes to rotating log file)
	logCloser, err := logger.Init(config.AppConfig.LogPath)
	if err != nil {
		log.Printf("⚠️ File logging disabled: %v", err)
	} else {
		defer logCloser.Close()
		log.Printf("📝 Logging to: %s (max 25MB per file)", config.AppConfig.LogPath)
	}

	if err := database.Connect(); err != nil {
		log.Printf("ERROR: Failed to connect to MongoDB: %v", err)
		time.Sleep(5 * time.Second)
		os.Exit(1)
	}
	defer database.Disconnect()
	log.Println("✅ MongoDB connected")

	// Check ffmpeg/ffprobe availability
	if err := transcoder.CheckFFmpeg(); err != nil {
		log.Printf("⚠️  ffmpeg not found — transcode will fail: %v", err)
	}
	if err := transcoder.CheckFFprobe(); err != nil {
		log.Printf("⚠️  ffprobe not found — probe will fail: %v", err)
	}

	// ── HTTP Server for Log Viewer ────────────────────────────
	port := config.AppConfig.Port
	if port == "" {
		port = "8081"
	}

	logDir := filepath.Dir(config.AppConfig.LogPath)
	h := handlers.NewHandler(handlers.Handler{LogDir: logDir})

	// Start WebSocket hub
	go handlers.GlobalHub.Run()

	// Start log file watcher (broadcasts changes to WS clients)
	go handlers.WatchLogDir(logDir)

	// Setup HTTP routes
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","service":"server-transcode","worker":"%s"}`, workerID)
	})

	mux.HandleFunc("/logs", h.HandleLogList)
	mux.HandleFunc("/logs/", h.HandleLogFile)
	mux.HandleFunc("/ui", h.HandleUI)
	mux.HandleFunc("/ws", h.HandleWS)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	go func() {
		ln, err := net.Listen("tcp", ":"+port)
		if err != nil {
			log.Printf("📋 Log viewer skipped (port %s in use by another worker)", port)
			return
		}
		server := &http.Server{
			Handler: middleware.CORS(mux),
		}
		log.Printf("🌐 Log viewer: http://localhost:%s/ui", port)
		if err := server.Serve(ln); err != http.ErrServerClosed {
			log.Printf("⚠️ HTTP server error: %v", err)
		}
	}()

	// ── Heartbeat ────────────────────────────────────────────
	go startHeartbeat(workerID)

	// ── Worker Loop ──────────────────────────────────────────
	startWorkerLoop()
}

// ─── Worker Loop ──────────────────────────────────────────────

func startWorkerLoop() {
	log.Println("⚡ Worker Mode: Polling for transcode jobs...")
	log.Printf("🆔 Worker ID: %s", workerID)

	utils.CleanOldLogs()

	ctx := context.Background()

	total, _ := models.FileModel.CountDocuments(ctx, bson.M{})
	ready, _ := models.FileModel.CountDocuments(ctx, bson.M{
		"status":             models.FileStatusReady,
		"type":               models.FileTypeVideo,
		"clonedFrom":         bson.M{"$exists": false},
		"metadata.trashedAt": bson.M{"$exists": false},
		"metadata.deletedAt": bson.M{"$exists": false},
	})
	log.Printf("📊 DB Stats: Total Files: %d, Ready Videos: %d", total, ready)

	// Check GPU and sort settings on startup
	checkGpuSetting(ctx)
	checkSortSetting(ctx)

	const (
		pollBusy = 5 * time.Second
		pollIdle = 30 * time.Second
	)

	for {
		if !isTranscodeEnabled(ctx) || !isWorkerEnabled(ctx) {
			time.Sleep(pollIdle)
			continue
		}
		checkGpuSetting(ctx)
		checkSortSetting(ctx)
		hadWork := processNextJob(ctx)
		if hadWork {
			time.Sleep(pollBusy)
		} else {
			time.Sleep(pollIdle)
		}
	}
}

// ─── Settings ─────────────────────────────────────────────────

func isTranscodeEnabled(ctx context.Context) bool {
	setting, err := models.SettingModel.FindOne(ctx, bson.M{"name": models.SettingTranscodeEnabled})
	if err != nil {
		if err == mongo.ErrNoDocuments {
			newSetting := models.SettingModel.New()
			newSetting.Name = models.SettingTranscodeEnabled
			newSetting.Value = false
			models.SettingModel.Create(ctx, newSetting)
			log.Println("⚙️  Created 'transcode_enabled' = false")
		}
		return false
	}
	return setting.GetBool(false)
}

func checkGpuSetting(ctx context.Context) {
	setting, err := models.SettingModel.FindOne(ctx, bson.M{"name": models.SettingGpuEnabled})
	if err != nil {
		if err == mongo.ErrNoDocuments {
			newSetting := models.SettingModel.New()
			newSetting.Name = models.SettingGpuEnabled
			newSetting.Value = true
			models.SettingModel.Create(ctx, newSetting)
			log.Println("⚙️  Created 'transcode_gpu_enabled' = true")
			transcoder.SetGPUEnabled(true)
			return
		}
		return
	}
	transcoder.SetGPUEnabled(setting.GetBool(true))
}

func checkSortSetting(ctx context.Context) {
	setting, err := models.SettingModel.FindOne(ctx, bson.M{"name": models.SettingTranscodeSort})
	if err != nil {
		if err == mongo.ErrNoDocuments {
			defaultSort := bson.M{"metadata.size": 1, "createdAt": 1}
			newSetting := models.SettingModel.New()
			newSetting.Name = models.SettingTranscodeSort
			newSetting.Value = defaultSort
			models.SettingModel.Create(ctx, newSetting)
			log.Printf("⚙️  Created 'transcode_sort' = %v", defaultSort)
		}
		transcodeSortOrder = bson.D{{Key: "metadata.size", Value: 1}, {Key: "createdAt", Value: 1}}
		return
	}

	// Parse sort value (stored as bson.M / map)
	if sortMap, ok := setting.Value.(bson.M); ok {
		var sortD bson.D
		for k, v := range sortMap {
			sortD = append(sortD, bson.E{Key: k, Value: v})
		}
		if len(sortD) > 0 {
			transcodeSortOrder = sortD
			return
		}
	}

	// Fallback
	transcodeSortOrder = bson.D{{Key: "metadata.size", Value: 1}, {Key: "createdAt", Value: 1}}
}

func isWorkerEnabled(ctx context.Context) bool {
	worker, err := models.WorkerModel.FindOne(ctx, bson.M{"workerId": workerID})
	if err != nil {
		return true // no record yet (before heartbeat creates it) → assume enabled
	}
	return worker.Enable
}

// ─── Job Discovery ────────────────────────────────────────────

func processNextJob(ctx context.Context) bool {
	// Debug: check current state of video_process for this worker
	allProcesses, _ := models.VideoProcessModel.Find(ctx, bson.M{
		"workerId":    workerID,
		"processType": models.ProcessTypeTranscode,
	})
	for _, p := range allProcesses {
		rc := 0
		if p.RetryCount != nil {
			rc = *p.RetryCount
		}
		log.Printf("🔍 processNextJob: existing process id=%s slug=%s status=%s retryCount=%d",
			p.ID, derefStr(p.Slug), derefStr(p.Status), rc)
	}
	if len(allProcesses) == 0 {
		log.Printf("🔍 processNextJob: no existing processes for this worker")
		// Check ALL documents in video_process (no filter) to see if record exists with different workerId
		allDocs, _ := models.VideoProcessModel.Find(ctx, bson.M{})
		if len(allDocs) > 0 {
			for _, d := range allDocs {
				log.Printf("🔍 GLOBAL: id=%s fileId=%s workerId=%s status=%s processType=%s",
					d.ID, derefStr(d.FileID), derefStr(d.WorkerID), derefStr(d.Status), d.ProcessType)
			}
		} else {
			log.Printf("🔍 GLOBAL: video_process collection is EMPTY (0 documents)")
		}
	}

	// 1. Cleanup permanently failed processes (retryCount >= 3)
	cleanupMaxRetryProcesses(ctx)

	// 2. Resume own interrupted or retry failed process
	if process := resumeOwnProcess(ctx); process != nil {
		slug := derefStr(process.Slug)
		if err := runTranscode(ctx, process); err != nil {
			log.Printf("❌ Resume failed: %s - %v", slug, err)
		}
		return true
	}

	log.Printf("🔍 processNextJob: no process to resume, looking for new file...")

	// 3. Find and claim new file
	process, file, err := findAndClaimFile(ctx)
	if err == nil && process != nil {
		slug := derefStr(process.Slug)
		log.Printf("🎬 New transcode: [%s] %s", slug, file.Name)
		if err := runTranscode(ctx, process); err != nil {
			log.Printf("❌ Failed: %s - %v", slug, err)
		}
		return true
	}

	return false
}

// ─── Resume Own Process ───────────────────────────────────────

func resumeOwnProcess(ctx context.Context) *models.VideoProcess {
	// 1. Resume interrupted process (status=processing) — no wait
	process, err := models.VideoProcessModel.FindOne(ctx, bson.M{
		"workerId":    workerID,
		"status":      models.ProcessStatusProcessing,
		"processType": models.ProcessTypeTranscode,
	})
	if err == nil {
		log.Printf("🔄 [%s] Resuming interrupted process", derefStr(process.Slug))
		return process
	}

	// 2. Retry failed process (retryCount < 3) — wait before retry
	failed, err := models.VideoProcessModel.FindOne(ctx, bson.M{
		"workerId":    workerID,
		"status":      models.ProcessStatusFailed,
		"processType": models.ProcessTypeTranscode,
		"retryCount":  bson.M{"$lt": 3},
	})
	if err == nil {
		slug := derefStr(failed.Slug)
		retryNum := 0
		if failed.RetryCount != nil {
			retryNum = *failed.RetryCount
		}

		// Backoff: 30s, 30s, 60s
		waitSec := 30
		if retryNum >= 2 {
			waitSec = 60
		}
		log.Printf("🔁 [%s] Retrying (attempt %d/3) — waiting %ds...", slug, retryNum+1, waitSec)
		time.Sleep(time.Duration(waitSec) * time.Second)

		// Reset status to processing for retry (raw MongoDB to avoid goose $set wrapping)
		models.VideoProcessModel.Col().UpdateOne(ctx,
			bson.M{"_id": failed.ID},
			bson.M{"$set": bson.M{
				"status":    models.ProcessStatusProcessing,
				"error":     "",
				"updatedAt": time.Now(),
			}},
		)
		status := models.ProcessStatusProcessing
		failed.Status = &status
		return failed
	}

	return nil
}

// ─── Max Retry Cleanup ────────────────────────────────────────

func cleanupMaxRetryProcesses(ctx context.Context) {
	processes, _ := models.VideoProcessModel.Find(ctx, bson.M{
		"workerId":    workerID,
		"status":      models.ProcessStatusFailed,
		"processType": models.ProcessTypeTranscode,
		"retryCount":  bson.M{"$gte": 3},
	})
	for _, pf := range processes {
		slug := derefStr(pf.Slug)
		fileID := derefStr(pf.FileID)
		log.Printf("🗑️  [%s] Max retries (3/3) — cleaning up", slug)

		removeDownloadDir(slug)

		// Delete the process record (file_errors will prevent future retries)
		models.VideoProcessModel.DeleteByID(ctx, pf.ID)

		// Insert file_error to prevent future retries
		errMsg := ""
		if pf.Error != nil {
			errMsg = *pf.Error
		}
		existing, _ := models.FileErrorModel.CountDocuments(ctx, bson.M{
			"fileId": fileID, "errorType": "transcode",
		})
		if existing == 0 {
			models.FileErrorModel.Create(ctx, &models.FileError{
				FileID:    fileID,
				ErrorType: "transcode",
				Error:     errMsg,
				Slug:      slug,
				WorkerID:  workerID,
			})
			log.Printf("🚫 [%s] Added to file_errors (transcode)", slug)
		}

		log.Printf("🗑️  [%s] Marked permanently_failed", slug)
	}
}

// ─── Shared Helpers ──────────────────────────────────────────

func removeDownloadDir(slug string) {
	exePath, _ := os.Executable()
	baseDir := filepath.Dir(exePath)
	if strings.Contains(exePath, "go-build") {
		baseDir, _ = os.Getwd()
	}
	downloadDir := filepath.Join(baseDir, "download", slug)
	os.RemoveAll(downloadDir)
}

// ─── Startup Cleanup ──────────────────────────────────────────

func cleanupStaleTempDirs(ctx context.Context) {
	exePath, _ := os.Executable()
	baseDir := filepath.Dir(exePath)
	if strings.Contains(exePath, "go-build") {
		baseDir, _ = os.Getwd()
	}
	downloadBase := filepath.Join(baseDir, "download")

	entries, err := os.ReadDir(downloadBase)
	if err != nil {
		return // no download dir yet
	}

	cleaned := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		slug := entry.Name()

		// Check if there's any active/retryable process for this slug
		count, _ := models.VideoProcessModel.CountDocuments(ctx, bson.M{
			"slug":   slug,
			"status": bson.M{"$in": []string{
				models.ProcessStatusProcessing,
				models.ProcessStatusFailed,
			}},
		})
		if count == 0 {
			dirPath := filepath.Join(downloadBase, slug)
			os.RemoveAll(dirPath)
			log.Printf("🧹 Startup cleanup: removed stale temp dir [%s]", slug)
			cleaned++
		}
	}

	if cleaned > 0 {
		log.Printf("🧹 Cleaned up %d stale temp dir(s)", cleaned)
	}
}
