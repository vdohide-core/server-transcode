# Server Transcode

Worker service สำหรับ transcode วิดีโอ (encode multi-resolution + upload) สำหรับ [VDOHide](https://vdohide.com)

## Features

- **Multi-Resolution Encoding** — encode 360p, 480p, 720p, 1080p จาก original media
- **GPU Acceleration** — รองรับ NVIDIA h264_nvenc (auto-detect, fallback เป็น CPU)
- **S3 Upload** — อัพโหลด transcoded files ขึ้น S3 temp storage (fallback เป็น SCP/local)
- **HTTP Download** — โหลด original จาก storage static server (`http://host:port/{slug}.mp4`)
- **SCP Upload** — fallback อัพโหลดไปยัง storage server ผ่าน SCP
- **Storage Fallback** — Permission denied → ลอง storage อื่นอัตโนมัติ
- **Multi-Worker** — รัน worker หลายตัวพร้อมกัน ด้วย `WORKER_ID`
- **Auto Retry** — retry อัตโนมัติ 3 ครั้ง พร้อม backoff (30s, 30s, 60s)
- **File Error Lock** — ไฟล์ที่ fail ถาวรจะถูกล็อคใน `file_errors` ไม่หยิบมาทำซ้ำ
- **Progress Tracking** — ติดตาม download/encode/upload ต่อ resolution ใน `video_process`
- **Resume Safe** — ตรวจสอบ file integrity ก่อน resume (size validation + re-encode corrupt files)
- **Clone Media** — clone media record ไปยังไฟล์ที่ clonedFrom อัตโนมัติ

## Requirements

- **FFmpeg** + **FFprobe** (ต้องอยู่ใน PATH)
- **Node.js** 18+ (สำหรับ SCP upload script)
- **MongoDB** (vdohide platform database)

---

## Installation (Linux Server)

### One-line install

```bash
curl -fsSL https://raw.githubusercontent.com/vdohide-core/server-transcode/main/install.sh | sudo -E bash -s -- \
    --mongodb-uri "mongodb+srv://user:pass@cluster.mongodb.net/platform" \
    -w 1
```

### Options

| Option | Default | คำอธิบาย |
|---|---|---|
| `-w, --count` | `1` | จำนวน worker instances |
| `--mongodb-uri` | `""` | MongoDB connection string |
| `--storage-id` | `""` | Storage ID (optional) |
| `--storage-path` | `/home/files` | Local storage path |
| `--node-version` | `22` | Node.js version |
| `--uninstall` | — | ถอนการติดตั้ง |

### Examples

```bash
# 2 workers
curl -fsSL https://raw.githubusercontent.com/vdohide-core/server-transcode/main/install.sh | sudo -E bash -s -- \
    --mongodb-uri "mongodb+srv://..." \
    --storage-path /home/files \
    -w 2

# Uninstall
curl -fsSL https://raw.githubusercontent.com/vdohide-core/server-transcode/main/install.sh | sudo bash -s -- --uninstall
```

### After install

```bash
# ดู logs
journalctl -u "server-transcode@*" -f

# ดู worker 1
journalctl -u "server-transcode@1" -f

# Restart workers
for i in $(seq 1 2); do systemctl restart server-transcode@$i; done

# Stop workers
for i in $(seq 1 2); do systemctl stop server-transcode@$i; done
```

---

## Download Latest Release

```bash
# Linux amd64
curl -L https://github.com/vdohide-core/server-transcode/releases/latest/download/linux -o server-transcode
chmod +x server-transcode

# Linux ARM64
curl -L https://github.com/vdohide-core/server-transcode/releases/latest/download/linux-arm64 -o server-transcode
chmod +x server-transcode

# Scripts (Node.js SCP)
curl -L https://github.com/vdohide-core/server-transcode/releases/latest/download/scripts.tar.gz | tar xz
cd scripts && npm install --production
```

---

## Configuration (.env)

```env
# Required
MONGODB_URI=mongodb+srv://user:pass@cluster.mongodb.net/platform

# Optional — ถ้าไม่ตั้งค่าจะใช้ storage จาก DB
STORAGE_ID=your-storage-uuid
STORAGE_PATH=/home/files

# Optional — Worker ID (default: transcode_hostname@1)
WORKER_ID=worker-1
```

---

## Development

```bash
# Clone
git clone https://github.com/vdohide-core/server-transcode.git
cd server-transcode

# ติดตั้ง Node.js dependencies (SCP script)
cd scripts && npm install && cd ..

# สร้าง .env
cp .env.example .env
# แก้ไข MONGODB_URI

# Run
go run ./cmd

# Build all platforms
./build.bat
```

---

## Release

```bash
git tag v1.2.0
git push origin v1.2.0
```

GitHub Actions จะ build และ release อัตโนมัติพร้อม:
- `linux` — Linux amd64 binary
- `linux-arm64` — Linux ARM64 binary  
- `scripts.tar.gz` — Node.js SCP scripts

---

## Architecture

```
Worker Loop (5s poll)
├── cleanupMaxRetryProcesses()    — retry ≥ 3 → file_errors + delete process
├── resumeOwnProcess()            — resume processing/failed ของ worker นี้
├── findAndClaimFile()             — หา ready video → exclude file_errors → claim
│   └── runTranscode()
│       ├── Download original      — SCP/local copy (size validation)
│       ├── Probe video info       — FFprobe
│       ├── Determine resolutions  — shortSide → 360/480/720/1080
│       ├── Per resolution:
│       │   ├── Encode (GPU/CPU)   — h264_nvenc or libx264
│       │   ├── Upload (SCP)       — fallback to alt storage on Permission denied
│       │   └── Create media       — media record + clone to cloned files
│       └── Update metadata.highest
└── findAndClaimFile() fail → sleep 30s → retry
```

## Timeline (per process)

```json
{
  "download":    { "status": "completed" },
  "encode_360":  { "status": "completed" },
  "upload_360":  { "status": "completed" },
  "encode_480":  { "status": "completed" },
  "upload_480":  { "status": "processing", "percent": 45 },
  "encode_720":  { "status": "pending" },
  "upload_720":  { "status": "pending" }
}
```

## Collections Used

| Collection | การใช้งาน |
|---|---|
| `files` | หา ready video, update status + metadata.highest |
| `video_process` | track download/encode/upload progress per resolution |
| `file_errors` | lock ไฟล์ที่ fail ถาวร (errorType: transcode) |
| `storages` | หา storage config (SSH, path) + fallback |
| `medias` | บันทึก processed media record per resolution |
| `workers` | heartbeat (type: transcode, status, system metrics) |
| `settings` | `transcode_enabled`, sort order |
