package uploader

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// SCPConfig contains the configuration for SCP upload
type SCPConfig struct {
	LocalPath  string `json:"localPath"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password,omitempty"`
	RemotePath string `json:"remotePath"`
	FileName   string `json:"fileName,omitempty"`
}

// SCPProgress represents a progress update from the Node.js script
type SCPProgress struct {
	Type        string `json:"type"`
	Percent     int    `json:"percent,omitempty"`
	Transferred int64  `json:"transferred,omitempty"`
	Total       int64  `json:"total,omitempty"`
	RemotePath  string `json:"remotePath,omitempty"`
	FileName    string `json:"fileName,omitempty"`
	FileSize    int64  `json:"fileSize,omitempty"`
	FileCount   int    `json:"fileCount,omitempty"`
	TotalSize   int64  `json:"totalSize,omitempty"`
	Message     string `json:"message,omitempty"`
	Host        string `json:"host,omitempty"`
}

// OnSCPProgress is a callback for SCP upload progress
type OnSCPProgress func(progress SCPProgress)

// UploadViaSCP uploads a file via SCP using the Node.js script
func UploadViaSCP(config SCPConfig, onProgress OnSCPProgress) error {
	if config.Port == 0 {
		config.Port = 22
	}

	scriptPath := findScriptPath()
	if scriptPath == "" {
		return fmt.Errorf("scp-upload.js script not found")
	}

	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	configBase64 := base64.StdEncoding.EncodeToString(configJSON)

	log.Printf("📤 SCP Upload: %s → %s:%s/%s", config.LocalPath, config.Host, config.RemotePath, config.FileName)

	cmd := exec.Command("node", scriptPath, configBase64)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start node: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	var lastProgress SCPProgress
	lastLogPercent := -1
	for scanner.Scan() {
		var progress SCPProgress
		if err := json.Unmarshal(scanner.Bytes(), &progress); err != nil {
			log.Printf("⚠️  SCP output parse error: %s", scanner.Text())
			continue
		}

		switch progress.Type {
		case "start":
			log.Printf("📤 SCP: Uploading %s (%.2f MB) to %s", progress.FileName, float64(progress.FileSize)/1024/1024, progress.Host)
		case "progress":
			if progress.Percent != lastLogPercent && (progress.Percent%10 == 0 || progress.Percent == 100) {
				lastLogPercent = progress.Percent
				log.Printf("📤 SCP: %d%% (%.2f / %.2f MB)", progress.Percent,
					float64(progress.Transferred)/1024/1024, float64(progress.Total)/1024/1024)
			}
		case "success":
			log.Printf("✅ SCP: Uploaded to %s (%.2f MB)", progress.RemotePath, float64(progress.FileSize)/1024/1024)
		case "error":
			log.Printf("❌ SCP: %s", progress.Message)
		}

		lastProgress = progress
		if onProgress != nil {
			onProgress(progress)
		}
	}

	if err := cmd.Wait(); err != nil {
		if lastProgress.Type == "error" {
			return fmt.Errorf("SCP failed: %s", lastProgress.Message)
		}
		return fmt.Errorf("SCP process failed: %w", err)
	}

	if lastProgress.Type != "success" {
		return fmt.Errorf("SCP did not complete successfully")
	}

	return nil
}

// SCPDirConfig contains the configuration for SCP directory upload
type SCPDirConfig struct {
	LocalDir   string `json:"localDir"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password,omitempty"`
	RemotePath string `json:"remotePath"`
}

// UploadDirViaSCP uploads an entire directory via SCP using scp-upload-dir.js
func UploadDirViaSCP(config SCPDirConfig) error {
	if config.Port == 0 {
		config.Port = 22
	}

	scriptPath := findDirUploadScriptPath()
	if scriptPath == "" {
		return fmt.Errorf("scp-upload-dir.js script not found")
	}

	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	configBase64 := base64.StdEncoding.EncodeToString(configJSON)

	log.Printf("📤 SCP Dir Upload: %s → %s:%s", config.LocalDir, config.Host, config.RemotePath)

	cmd := exec.Command("node", scriptPath, configBase64)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start node: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	var lastProgress SCPProgress
	for scanner.Scan() {
		var progress SCPProgress
		if err := json.Unmarshal(scanner.Bytes(), &progress); err != nil {
			continue
		}

		switch progress.Type {
		case "start":
			log.Printf("📤 SCP Dir: Uploading %d files (%.2f MB) to %s",
				progress.FileCount, float64(progress.TotalSize)/1024/1024, progress.Host)
		case "success":
			log.Printf("✅ SCP Dir: Uploaded %d files to %s", progress.FileCount, progress.RemotePath)
		case "error":
			log.Printf("❌ SCP Dir: %s", progress.Message)
		}
		lastProgress = progress
	}

	if err := cmd.Wait(); err != nil {
		if lastProgress.Type == "error" {
			return fmt.Errorf("SCP dir upload failed: %s", lastProgress.Message)
		}
		return fmt.Errorf("SCP dir upload process failed: %w", err)
	}

	if lastProgress.Type != "success" {
		return fmt.Errorf("SCP dir upload did not complete successfully")
	}

	return nil
}

// SCPDownloadConfig contains the configuration for SCP download
type SCPDownloadConfig struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password,omitempty"`
	RemotePath string `json:"remotePath"`
	LocalPath  string `json:"localPath"`
}

// DownloadViaSCP downloads a file via SCP using the Node.js script
func DownloadViaSCP(config SCPDownloadConfig, onProgress OnSCPProgress) error {
	if config.Port == 0 {
		config.Port = 22
	}

	scriptPath := findDownloadScriptPath()
	if scriptPath == "" {
		return fmt.Errorf("scp-download.js script not found")
	}

	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	configBase64 := base64.StdEncoding.EncodeToString(configJSON)

	log.Printf("📥 SCP Download: %s:%s → %s", config.Host, config.RemotePath, config.LocalPath)

	cmd := exec.Command("node", scriptPath, configBase64)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start node: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	var lastProgress SCPProgress
	lastLogPercent := -1
	for scanner.Scan() {
		var progress SCPProgress
		if err := json.Unmarshal(scanner.Bytes(), &progress); err != nil {
			log.Printf("⚠️  SCP output parse error: %s", scanner.Text())
			continue
		}

		switch progress.Type {
		case "start":
			log.Printf("📥 SCP: Downloading from %s", config.Host)
		case "progress":
			if progress.Percent != lastLogPercent && (progress.Percent%10 == 0 || progress.Percent == 100) {
				lastLogPercent = progress.Percent
				log.Printf("📥 SCP: %d%% (%.2f / %.2f MB)", progress.Percent,
					float64(progress.Transferred)/1024/1024, float64(progress.Total)/1024/1024)
			}
		case "success":
			log.Printf("✅ SCP: Downloaded to %s (%.2f MB)", config.LocalPath, float64(progress.FileSize)/1024/1024)
		case "error":
			log.Printf("❌ SCP: %s", progress.Message)
		}

		lastProgress = progress
		if onProgress != nil {
			onProgress(progress)
		}
	}

	if err := cmd.Wait(); err != nil {
		if lastProgress.Type == "error" {
			return fmt.Errorf("SCP download failed: %s", lastProgress.Message)
		}
		return fmt.Errorf("SCP download process failed: %w", err)
	}

	if lastProgress.Type != "success" {
		return fmt.Errorf("SCP download did not complete successfully")
	}

	return nil
}

// ─── Script Path Finders ─────────────────────────────────────

func findScriptPath() string {
	return findScript("scp-upload.js")
}

func findDirUploadScriptPath() string {
	return findScript("scp-upload-dir.js")
}

func findDownloadScriptPath() string {
	return findScript("scp-download.js")
}

// findScript finds a script file by name relative to the executable, cwd, or GOPATH
func findScript(name string) string {
	// Try relative to executable
	exePath, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exePath)
		for _, dir := range []string{exeDir, filepath.Join(exeDir, "..")} {
			c := filepath.Join(dir, "scripts", name)
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}

	// Try relative to working directory
	cwd, err := os.Getwd()
	if err == nil {
		for _, dir := range []string{cwd, filepath.Join(cwd, "..")} {
			c := filepath.Join(dir, "scripts", name)
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}

	// Try GOPATH for development
	if runtime.GOOS != "" {
		gopath := os.Getenv("GOPATH")
		if gopath != "" {
			candidate := filepath.Join(gopath, "src", "server-transcode", "scripts", name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}

	return ""
}
