package transcoder

import (
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SpriteResult contains the result of sprite thumbnail generation
type SpriteResult struct {
	SpriteDir   string   // absolute path to sprite/ directory
	SpriteFiles []string // filenames only: 1.jpg, 2.jpg, ..., sprite.vtt
	VTTFile     string   // filename: sprite.vtt
}

// GenerateSpriteThumbnails generates sprite sheet thumbnails from a video file
// Output goes into {outputDir}/sprite/ folder with files named 1.jpg, 2.jpg, ...
// Grid: 6 columns, rows calculated from actual frames (no black padding)
// Interval: 2 seconds per frame
func GenerateSpriteThumbnails(inputPath, outputDir string, duration float64) (*SpriteResult, error) {
	if duration <= 0 {
		return nil, fmt.Errorf("invalid duration: %.2f", duration)
	}

	// Create sprite subdirectory
	spriteDir := filepath.Join(outputDir, "sprite")
	os.MkdirAll(spriteDir, 0755)

	// Fixed interval: 2 seconds per frame (industry standard)
	cols := 6
	maxRows := 6
	framesPerSheet := cols * maxRows
	interval := 2.0

	totalFrames := int(math.Floor(duration / interval))
	if totalFrames < 1 {
		totalFrames = 1
	}

	// Get thumbnail dimensions from input (base size 160px on the long side)
	thumbBase := 160
	var thumbWidth, thumbHeight int

	videoInfo, err := ProbeVideoInfo(inputPath)
	if err == nil && videoInfo.Width > 0 && videoInfo.Height > 0 {
		if videoInfo.Width > videoInfo.Height {
			// Landscape: width = 160, height = proportional
			thumbWidth = thumbBase
			thumbHeight = int(float64(thumbBase) * float64(videoInfo.Height) / float64(videoInfo.Width))
		} else if videoInfo.Height > videoInfo.Width {
			// Portrait: height = 160, width = proportional
			thumbHeight = thumbBase
			thumbWidth = int(float64(thumbBase) * float64(videoInfo.Width) / float64(videoInfo.Height))
		} else {
			// Square: 160x160
			thumbWidth = thumbBase
			thumbHeight = thumbBase
		}
		// Ensure even dimensions
		if thumbWidth%2 != 0 {
			thumbWidth++
		}
		if thumbHeight%2 != 0 {
			thumbHeight++
		}
	} else {
		thumbWidth = thumbBase
		thumbHeight = 90 // default 16:9 for 160px width
	}

	log.Printf("🧠 Generating %d thumbnails (interval = %.0fs, size = %dx%d)", totalFrames, interval, thumbWidth, thumbHeight)

	// Step 1: Generate sprite sheets using ffmpeg tile filter (fast, single pass)
	fpsFilter := fmt.Sprintf("fps=1/%.2f", interval)
	scaleFilter := fmt.Sprintf("scale=%d:%d", thumbWidth, thumbHeight)
	tileFilter := fmt.Sprintf("tile=%dx%d", cols, maxRows)

	spritePattern := filepath.Join(spriteDir, "%d.jpg")

	cmd := exec.Command("ffmpeg",
		"-y",
		"-i", inputPath,
		"-vf", fmt.Sprintf("%s,%s,%s", fpsFilter, scaleFilter, tileFilter),
		"-q:v", "9",
		spritePattern,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sprite generation failed: %w\n%s", err, string(output))
	}

	// Step 2: Find generated sprite files
	var spriteFiles []string
	for i := 1; i <= 1000; i++ {
		name := fmt.Sprintf("%d.jpg", i)
		path := filepath.Join(spriteDir, name)
		if _, err := os.Stat(path); err == nil {
			spriteFiles = append(spriteFiles, name)
		} else {
			break
		}
	}

	if len(spriteFiles) == 0 {
		return nil, fmt.Errorf("no sprite files generated")
	}

	// Step 2.5: Probe the first sprite sheet to get actual cell dimensions
	firstSpritePath := filepath.Join(spriteDir, spriteFiles[0])
	actualW, actualH := probeImageDimensions(firstSpritePath)
	if actualW > 0 && actualH > 0 {
		realThumbW := actualW / cols
		realThumbH := actualH / maxRows
		if realThumbW != thumbWidth || realThumbH != thumbHeight {
			log.Printf("⚠️  Sprite cell size mismatch: calculated=%dx%d, actual=%dx%d — using actual",
				thumbWidth, thumbHeight, realThumbW, realThumbH)
			thumbWidth = realThumbW
			thumbHeight = realThumbH
		}
	}

	// Step 3: Crop the LAST sprite sheet to remove black padding rows
	sheetsCount := len(spriteFiles)
	framesInLastSheet := totalFrames - (sheetsCount-1)*framesPerSheet
	if framesInLastSheet <= 0 {
		framesInLastSheet = 1
	}

	lastSheetRows := int(math.Ceil(float64(framesInLastSheet) / float64(cols)))
	if lastSheetRows < maxRows && lastSheetRows > 0 {
		lastSpriteName := spriteFiles[sheetsCount-1]
		lastSpritePath := filepath.Join(spriteDir, lastSpriteName)
		croppedPath := filepath.Join(spriteDir, "cropped_last.jpg")

		cropW := thumbWidth * cols
		cropH := thumbHeight * lastSheetRows

		cropCmd := exec.Command("ffmpeg",
			"-y",
			"-i", lastSpritePath,
			"-vf", fmt.Sprintf("crop=%d:%d:0:0", cropW, cropH),
			"-q:v", "5",
			croppedPath,
		)

		cropOutput, cropErr := cropCmd.CombinedOutput()
		if cropErr != nil {
			log.Printf("⚠️ Failed to crop last sprite sheet, keeping original: %s", string(cropOutput))
		} else {
			os.Remove(lastSpritePath)
			os.Rename(croppedPath, lastSpritePath)
			log.Printf("✂️  Cropped last sprite sheet to %d rows (was %d)", lastSheetRows, maxRows)
		}
	}

	log.Printf("🖼️  Generated %d sprite sheets (%d total frames)", sheetsCount, totalFrames)

	// Step 4: Generate sprite.vtt with actual frame count per sheet
	vttContent := generateSpriteVTT(spriteFiles, interval, thumbWidth, thumbHeight, totalFrames, cols, framesPerSheet)
	vttPath := filepath.Join(spriteDir, "sprite.vtt")
	if err := os.WriteFile(vttPath, []byte(vttContent), 0644); err != nil {
		return nil, fmt.Errorf("failed to write sprite.vtt: %w", err)
	}

	// Add sprite.vtt to files list
	spriteFiles = append(spriteFiles, "sprite.vtt")

	return &SpriteResult{
		SpriteDir:   spriteDir,
		SpriteFiles: spriteFiles,
		VTTFile:     "sprite.vtt",
	}, nil
}

// generateSpriteVTT generates WebVTT content for sprite thumbnails
// Compatible with JW Player preview thumbnails
func generateSpriteVTT(spriteFiles []string, interval float64, thumbWidth, thumbHeight, totalFrames, cols, framesPerSheet int) string {
	var sb strings.Builder
	sb.WriteString("WEBVTT\n\n")

	frameIndex := 0

	for _, spriteFileName := range spriteFiles {
		remaining := totalFrames - frameIndex
		framesInSheet := framesPerSheet
		if remaining < framesPerSheet {
			framesInSheet = remaining
		}
		if framesInSheet <= 0 {
			break
		}

		for i := 0; i < framesInSheet; i++ {
			col := i % cols
			row := i / cols

			startTime := float64(frameIndex) * interval
			endTime := float64(frameIndex+1) * interval

			x := col * thumbWidth
			y := row * thumbHeight

			sb.WriteString(fmt.Sprintf("%s --> %s\n",
				formatVTTTime(startTime),
				formatVTTTime(endTime),
			))
			sb.WriteString(fmt.Sprintf("%s#xywh=%d,%d,%d,%d\n\n",
				spriteFileName, x, y, thumbWidth, thumbHeight,
			))

			frameIndex++
		}
	}

	return sb.String()
}

// probeImageDimensions gets the pixel dimensions of an image file using ffprobe
func probeImageDimensions(imagePath string) (int, int) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=s=x:p=0",
		imagePath,
	)
	output, err := cmd.Output()
	if err != nil {
		return 0, 0
	}
	parts := strings.Split(strings.TrimSpace(string(output)), "x")
	if len(parts) != 2 {
		return 0, 0
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return w, h
}

// formatVTTTime formats seconds to WebVTT time format "HH:MM:SS.mmm"
func formatVTTTime(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	hours := int(seconds) / 3600
	minutes := (int(seconds) % 3600) / 60
	secs := int(seconds) % 60
	millis := int((seconds - float64(int(seconds))) * 1000)

	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, secs, millis)
}
