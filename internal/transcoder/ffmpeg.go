package transcoder

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// isLinux returns true if running on Linux
func isLinux() bool {
	return runtime.GOOS == "linux"
}

// GPU encoder types
const (
	EncoderCPU   = "libx264"
	EncoderNVENC = "h264_nvenc"
	EncoderAMF   = "h264_amf"
	EncoderQSV   = "h264_qsv"
)

var (
	detectedEncoder string
	gpuEnabled      bool
	encoderDetected bool
	encoderMu       sync.Mutex
)

// SetGPUEnabled sets whether GPU encoding is allowed
// Called from main loop when reading transcode_gpu_enabled setting
func SetGPUEnabled(enabled bool) {
	encoderMu.Lock()
	defer encoderMu.Unlock()

	if gpuEnabled != enabled {
		gpuEnabled = enabled
		encoderDetected = false // re-detect on next call
		if enabled {
			log.Printf("🎮 GPU encoding enabled — will auto-detect on next encode")
		} else {
			detectedEncoder = EncoderCPU
			encoderDetected = true
			log.Printf("💻 GPU encoding disabled — using CPU (libx264)")
		}
	}
}

// DetectEncoder detects the best available encoder
// If GPU is disabled, always returns CPU
func DetectEncoder() string {
	encoderMu.Lock()
	defer encoderMu.Unlock()

	if encoderDetected {
		return detectedEncoder
	}

	if !gpuEnabled {
		detectedEncoder = EncoderCPU
		encoderDetected = true
		log.Printf("💻 GPU disabled — using CPU (libx264)")
		return detectedEncoder
	}

	// Try NVIDIA first (most common)
	if testEncoder(EncoderNVENC) {
		detectedEncoder = EncoderNVENC
		encoderDetected = true
		log.Printf("🎮 GPU Detected: NVIDIA (h264_nvenc)")
		return detectedEncoder
	}

	// Try AMD
	if testEncoder(EncoderAMF) {
		detectedEncoder = EncoderAMF
		encoderDetected = true
		log.Printf("🎮 GPU Detected: AMD (h264_amf)")
		return detectedEncoder
	}

	// Try Intel QuickSync
	if testEncoder(EncoderQSV) {
		detectedEncoder = EncoderQSV
		encoderDetected = true
		log.Printf("🎮 GPU Detected: Intel QSV (h264_qsv)")
		return detectedEncoder
	}

	// Fallback to CPU
	detectedEncoder = EncoderCPU
	encoderDetected = true
	log.Printf("💻 No GPU encoder found — using CPU (libx264)")
	return detectedEncoder
}

// testEncoder tests if an encoder is available by running a quick encode
func testEncoder(encoder string) bool {
	var args []string

	switch encoder {
	case EncoderNVENC:
		// NVENC needs proper pixel format — use color source with nv12
		args = []string{
			"-hide_banner",
			"-loglevel", "error",
			"-f", "lavfi",
			"-i", "color=c=black:s=256x256:d=1:r=25",
			"-pix_fmt", "yuv420p",
			"-c:v", encoder,
			"-frames:v", "1",
			"-f", "null",
			"-",
		}
	default:
		args = []string{
			"-hide_banner",
			"-loglevel", "error",
			"-f", "lavfi",
			"-i", "color=c=black:s=256x256:d=1:r=25",
			"-c:v", encoder,
			"-frames:v", "1",
			"-f", "null",
			"-",
		}
	}

	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("   ❌ Test %s failed: %v — %s", encoder, err, strings.TrimSpace(string(output)))
		return false
	}
	log.Printf("   ✅ Test %s passed", encoder)
	return true
}

// getCRF returns CRF/CQ value based on resolution
// Standard streaming quality — lower = better quality, larger file
func getCRF(shortSide int) int {
	if shortSide >= 1080 {
		return 24
	}
	if shortSide >= 720 {
		return 25
	}
	if shortSide >= 480 {
		return 26
	}
	return 27
}

// getBitrateForRes returns target bitrate, maxrate, bufsize for a resolution
// Balanced for streaming — close to Netflix H.264 quality
func getBitrateForRes(shortSide int) (bitrate string, maxrate string, bufsize string) {
	switch {
	case shortSide >= 1080:
		return "3500k", "5250k", "7000k"
	case shortSide >= 720:
		return "2000k", "3000k", "4000k"
	case shortSide >= 480:
		return "1000k", "1500k", "2000k"
	default: // 360p
		return "600k", "900k", "1200k"
	}
}

// getAudioBitrate returns audio bitrate per resolution
func getAudioBitrate(shortSide int) string {
	switch {
	case shortSide >= 1080:
		return "128k"
	case shortSide >= 360:
		return "96k"
	default:
		return "64k"
	}
}

// getEncoderArgs returns ffmpeg encoder arguments based on detected encoder
func getEncoderArgs(encoder string, shortSide int) []string {
	crf := getCRF(shortSide)
	bitrate, maxrate, bufsize := getBitrateForRes(shortSide)

	switch encoder {
	case EncoderNVENC:
		return []string{
			"-c:v", "h264_nvenc",
			"-preset", "p5",
			"-tune", "hq",
			"-rc", "vbr",
			"-cq", strconv.Itoa(crf),
			"-b:v", bitrate,
			"-maxrate", maxrate,
			"-bufsize", bufsize,
			"-profile:v", "high",
			"-spatial-aq", "1",
			"-b_ref_mode", "middle",
		}
	case EncoderAMF:
		qp := crf + 2
		return []string{
			"-c:v", "h264_amf",
			"-quality", "quality",
			"-rc", "cqp",
			"-qp_i", strconv.Itoa(qp),
			"-qp_p", strconv.Itoa(qp + 2),
			"-profile:v", "high",
		}
	case EncoderQSV:
		return []string{
			"-c:v", "h264_qsv",
			"-preset", "slow",
			"-global_quality", strconv.Itoa(crf),
			"-look_ahead", "1",
			"-maxrate", maxrate,
			"-bufsize", bufsize,
			"-profile:v", "high",
		}
	default: // CPU
		return []string{
			"-c:v", "libx264",
			"-preset", "medium",
			"-crf", strconv.Itoa(crf),
			"-profile:v", "high",
			"-threads", "0",
			"-x264-params", "keyint=60:min-keyint=30:scenecut=40",
		}
	}
}

// EncodeResolution encodes a video file to a specific resolution
// Auto-detects GPU encoder, fallback to CPU
func EncodeResolution(inputPath, outputPath string, targetW, targetH int, totalDuration float64, onProgress func(percent int)) error {
	encoder := DetectEncoder()
	scaleFilter := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=ceil(iw/2)*2:ceil(ih/2)*2", targetW, targetH)

	// Determine short side for per-resolution settings
	shortSide := targetH
	if targetW < targetH {
		shortSide = targetW
	}
	audioBitrate := getAudioBitrate(shortSide)

	// Build ffmpeg command
	args := []string{"-y"}

	// Use GPU hwaccel for decode — reduces CPU usage significantly
	switch encoder {
	case EncoderNVENC:
		args = append(args, "-hwaccel", "cuda")
	case EncoderAMF:
		if isLinux() {
			args = append(args, "-hwaccel", "vaapi")
		} else {
			args = append(args, "-hwaccel", "d3d11va")
		}
	case EncoderQSV:
		args = append(args, "-hwaccel", "qsv")
	}

	args = append(args, "-i", inputPath)

	// Add encoder-specific args (includes bitrate/maxrate for GPU)
	args = append(args, getEncoderArgs(encoder, shortSide)...)

	// Always use CPU-based scale filter (compatible with all encoders)
	args = append(args, "-vf", scaleFilter)
	args = append(args, "-pix_fmt", "yuv420p")

	args = append(args,
		"-c:a", "aac",
		"-b:a", audioBitrate,
		"-ac", "2",
		"-ar", "48000",
		"-movflags", "+faststart",
		outputPath,
	)

	log.Printf("🔧 Encoder: %s | CRF/CQ: %d | Audio: %s", encoder, getCRF(shortSide), audioBitrate)

	cmd := exec.Command("ffmpeg", args...)

	err := runFFmpegWithProgress(cmd, totalDuration, onProgress)
	if err != nil {
		// If GPU encoder fails, retry with CPU
		if encoder != EncoderCPU {
			log.Printf("⚠️  GPU encode failed, retrying with CPU (libx264)...")
			detectedEncoder = EncoderCPU // force CPU for subsequent encodes

			cpuArgs := []string{"-y", "-i", inputPath}
			cpuArgs = append(cpuArgs, getEncoderArgs(EncoderCPU, shortSide)...)
			cpuArgs = append(cpuArgs,
				"-vf", scaleFilter,
				"-c:a", "aac",
				"-b:a", audioBitrate,
				"-ac", "2",
				"-ar", "48000",
				"-movflags", "+faststart",
				"-pix_fmt", "yuv420p",
				outputPath,
			)

			cmd2 := exec.Command("ffmpeg", cpuArgs...)
			err = runFFmpegWithProgress(cmd2, totalDuration, onProgress)
			if err != nil {
				return fmt.Errorf("encode failed (CPU fallback): %w", err)
			}
		} else {
			return fmt.Errorf("encode failed: %w", err)
		}
	}

	// Verify output
	info, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("output file not found: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("output file is empty")
	}

	log.Printf("✅ Encoded %s (%.2f MB) [%s]", outputPath, float64(info.Size())/1024/1024, encoder)
	return nil
}

// CheckFFmpeg verifies ffmpeg is available
func CheckFFmpeg() error {
	cmd := exec.Command("ffmpeg", "-version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg not found: %w", err)
	}
	return nil
}

// CheckFFprobe verifies ffprobe is available
func CheckFFprobe() error {
	cmd := exec.Command("ffprobe", "-version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffprobe not found: %w", err)
	}
	return nil
}

// runFFmpegWithProgress runs ffmpeg and parses stderr for progress
func runFFmpegWithProgress(cmd *exec.Cmd, totalDuration float64, onProgress func(percent int)) error {
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to pipe stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	lastPercent := -1
	var lastLines []string
	const maxLastLines = 10

	scanner := bufio.NewScanner(stderr)
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		for i, b := range data {
			if b == '\r' || b == '\n' {
				return i + 1, data[:i], nil
			}
		}
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for scanner.Scan() {
			line := scanner.Text()
			if len(strings.TrimSpace(line)) == 0 {
				continue
			}

			lastLines = append(lastLines, line)
			if len(lastLines) > maxLastLines {
				lastLines = lastLines[1:]
			}

			if idx := strings.Index(line, "time="); idx >= 0 {
				timeStr := line[idx+5:]
				if spaceIdx := strings.IndexAny(timeStr, " \t"); spaceIdx > 0 {
					timeStr = timeStr[:spaceIdx]
				}

				currentSec := parseTimeToSeconds(timeStr)
				if currentSec > 0 && totalDuration > 0 {
					percent := int(currentSec / totalDuration * 100)
					if percent > 100 {
						percent = 100
					}
					if percent >= lastPercent+1 {
						lastPercent = percent
						if onProgress != nil {
							onProgress(percent)
						}
					}
				}
			}
		}
	}()

	<-done
	waitErr := cmd.Wait()

	if waitErr != nil {
		var stderrMsg string
		if len(lastLines) > 0 {
			log.Printf("⚠️  ffmpeg stderr (last %d lines):", len(lastLines))
			for _, line := range lastLines {
				log.Printf("   %s", line)
			}
			stderrMsg = strings.Join(lastLines, "\n")
		}
		return fmt.Errorf("%w: %s", waitErr, stderrMsg)
	}

	return nil
}

// parseTimeToSeconds converts "HH:MM:SS.xx" to seconds
func parseTimeToSeconds(timeStr string) float64 {
	if strings.HasPrefix(timeStr, "-") {
		return 0
	}

	parts := strings.Split(timeStr, ":")
	if len(parts) != 3 {
		return 0
	}

	hours, _ := strconv.ParseFloat(parts[0], 64)
	minutes, _ := strconv.ParseFloat(parts[1], 64)
	seconds, _ := strconv.ParseFloat(parts[2], 64)

	return hours*3600 + minutes*60 + seconds
}

// GetFileSize returns the file size in bytes
func GetFileSize(filePath string) int64 {
	info, err := os.Stat(filePath)
	if err != nil {
		return 0
	}
	return info.Size()
}
