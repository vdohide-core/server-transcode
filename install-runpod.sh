#!/bin/bash

# Server Transcode — RunPod Installation Script
# Designed for RunPod GPU Pods (Docker containers, no systemd)
#
# Usage:
#   1. SSH into RunPod pod
#   2. git clone https://github.com/vdohide-core/server-transcode-releases.git /workspace/server-transcode
#   3. cd /workspace/server-transcode
#   4. chmod +x install-runpod.sh
#   5. ./install-runpod.sh --mongodb-uri "mongodb+srv://..." -n 1
#
# Or one-liner:
#   curl -fsSL https://raw.githubusercontent.com/vdohide-core/server-transcode-releases/main/install-runpod.sh | bash -s -- --mongodb-uri "mongodb+srv://..."

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

# Defaults
WORKER_COUNT=1
MONGODB_URI=""
STORAGE_ID=""
STORAGE_PATH=""
NODE_VERSION="22"
SKIP_FFMPEG=false
STOP_ONLY=false
UNINSTALL=false

APP_NAME="server-transcode"
APP_DIR="/workspace/server-transcode"
URL_BASE="https://github.com/vdohide-core/server-transcode/releases/latest/download"
PID_DIR="$APP_DIR/pids"
LOG_DIR="$APP_DIR/log"

# Functions
print_status() { echo -e "${GREEN}[INFO]${NC} $1"; }
print_warning() { echo -e "${YELLOW}[WARN]${NC} $1"; }
print_error() { echo -e "${RED}[ERROR]${NC} $1"; }
print_header() { echo -e "${CYAN}$1${NC}"; }

show_help() {
    echo "Server Transcode — RunPod Installer"
    echo ""
    echo "Usage: ./install-runpod.sh [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  -n, --count NUM        Number of worker instances (default: 1)"
    echo "  --mongodb-uri URI      MongoDB connection string (required on first install)"
    echo "  --storage-id ID        Storage ID (optional, for local storage mode)"
    echo "  --storage-path DIR     Storage path (optional, for local storage mode)"
    echo "  --node-version VER     Node.js version (default: 22)"
    echo "  --skip-ffmpeg          Skip FFmpeg installation (if already installed)"
    echo "  --stop                 Stop all running workers and exit"
    echo "  --uninstall            Uninstall everything and clean up"
    echo "  -h, --help             Show this help message"
    echo ""
    echo "Examples:"
    echo "  # Install with 1 worker"
    echo "  ./install-runpod.sh --mongodb-uri \"mongodb+srv://user:pass@host/db\""
    echo ""
    echo "  # Install with 2 workers"
    echo "  ./install-runpod.sh --mongodb-uri \"mongodb+srv://user:pass@host/db\" -n 2"
    echo ""
    echo "  # Stop all workers"
    echo "  ./install-runpod.sh --stop"
    echo ""
    echo "  # Uninstall everything"
    echo "  ./install-runpod.sh --uninstall"
    exit 0
}

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        -n|--count) WORKER_COUNT="$2"; shift 2;;
        --mongodb-uri) MONGODB_URI="$2"; shift 2;;
        --storage-id) STORAGE_ID="$2"; shift 2;;
        --storage-path) STORAGE_PATH="$2"; shift 2;;
        --node-version) NODE_VERSION="$2"; shift 2;;
        --skip-ffmpeg) SKIP_FFMPEG=true; shift;;
        --stop) STOP_ONLY=true; shift;;
        --uninstall) UNINSTALL=true; shift;;
        -h|--help) show_help;;
        *) print_error "Unknown option: $1"; exit 1;;
    esac
done

# ==========================================
# Stop workers
# ==========================================
stop_workers() {
    print_status "Stopping all workers..."
    if [ -d "$PID_DIR" ]; then
        for pidfile in "$PID_DIR"/*.pid; do
            [ -f "$pidfile" ] || continue
            pid=$(cat "$pidfile")
            name=$(basename "$pidfile" .pid)
            if kill -0 "$pid" 2>/dev/null; then
                kill "$pid" 2>/dev/null || true
                print_status "Stopped $name (PID: $pid)"
            else
                print_warning "$name (PID: $pid) was not running"
            fi
            rm -f "$pidfile"
        done
    fi
    # Also kill any remaining server-transcode processes
    pkill -f "$APP_DIR/$APP_NAME" 2>/dev/null || true
    print_status "All workers stopped."
}

# ==========================================
# Uninstall
# ==========================================
uninstall() {
    echo ""
    print_header "============================================"
    print_header "  Server Transcode — Uninstaller"
    print_header "============================================"
    echo ""

    # 1. Stop all workers
    print_status "[1/5] Stopping all workers..."
    stop_workers

    # 2. Remove application directory
    print_status "[2/5] Removing application directory..."
    if [ -d "$APP_DIR" ]; then
        rm -rf "$APP_DIR"
        print_status "Removed $APP_DIR"
    else
        print_warning "$APP_DIR not found — skipping."
    fi

    # 3. Remove FFmpeg (only the static build we installed)
    print_status "[3/5] Removing FFmpeg (static build)..."
    if [ -f "/usr/local/bin/ffmpeg" ]; then
        rm -f /usr/local/bin/ffmpeg
        rm -f /usr/local/bin/ffprobe
        print_status "Removed /usr/local/bin/ffmpeg and /usr/local/bin/ffprobe"
    else
        print_warning "FFmpeg not found at /usr/local/bin — skipping."
    fi

    # 4. Remove Node.js (installed via nodesource)
    print_status "[4/5] Removing Node.js..."
    if command -v node &>/dev/null; then
        apt-get remove -y -qq nodejs 2>/dev/null || true
        apt-get autoremove -y -qq 2>/dev/null || true
        # Remove nodesource list
        rm -f /etc/apt/sources.list.d/nodesource.list 2>/dev/null || true
        rm -f /etc/apt/keyrings/nodesource.gpg 2>/dev/null || true
        print_status "Node.js removed."
    else
        print_warning "Node.js not found — skipping."
    fi

    # 5. Clean up temp files
    print_status "[5/5] Cleaning up..."
    rm -rf /tmp/ffmpeg-build 2>/dev/null || true

    echo ""
    print_header "============================================"
    print_header "  ✅ Uninstallation completed!"
    print_header "============================================"
    echo ""
    echo "  Removed:"
    echo "    • All worker processes"
    echo "    • Application directory ($APP_DIR)"
    echo "    • FFmpeg static build (/usr/local/bin/ffmpeg)"
    echo "    • Node.js"
    echo "    • Temporary files"
    echo ""
    print_header "============================================"
}

if [ "$STOP_ONLY" = true ]; then
    stop_workers
    exit 0
fi

if [ "$UNINSTALL" = true ]; then
    uninstall
    exit 0
fi

# ==========================================
# Header
# ==========================================
echo ""
print_header "============================================"
print_header "  Server Transcode — RunPod Installer"
print_header "============================================"
echo ""
print_status "Workers: $WORKER_COUNT"
print_status "App dir: $APP_DIR"
echo ""

# ==========================================
# Check if we're on RunPod
# ==========================================
if [ -d "/workspace" ]; then
    print_status "RunPod workspace detected ✓"
else
    print_warning "Not on RunPod? /workspace not found. Creating it..."
    mkdir -p /workspace
fi

# ==========================================
# Install system dependencies
# ==========================================
print_status "Installing system dependencies..."
apt-get update -qq 2>/dev/null
apt-get install -y -qq curl jq wget 2>/dev/null
print_status "System dependencies installed."

# ==========================================
# Install FFmpeg
# ==========================================
NEED_FFMPEG=false
if [ "$SKIP_FFMPEG" = true ]; then
    print_status "Skipping FFmpeg installation (--skip-ffmpeg)"
elif command -v ffmpeg &>/dev/null && ffmpeg -encoders 2>/dev/null | grep -q h264_nvenc; then
    print_status "FFmpeg with NVENC already installed ✓"
else
    NEED_FFMPEG=true
fi

if [ "$NEED_FFMPEG" = true ]; then
    print_status "Installing FFmpeg (Stable APT version) for RunPod compatibility..."
    
    # RunPod host drivers sometimes conflict with static bleeding-edge builds.
    # Standard apt version works best across all RunPod GPU templates.
    apt-get update -qq 2>/dev/null
    apt-get install -y -qq ffmpeg 2>/dev/null

    # Verify
    if ffmpeg -encoders 2>/dev/null | grep -q h264_nvenc; then
        print_status "FFmpeg with NVENC installed ✓"
    else
        print_warning "FFmpeg installed but NVENC not detected. GPU encoding may fall back to CPU."
    fi
fi

print_status "FFmpeg: $(ffmpeg -version 2>/dev/null | head -1)"

# ==========================================
# Check GPU & NVENC
# ==========================================
print_status "Checking GPU..."
NVENC_OK=false
if command -v nvidia-smi &>/dev/null; then
    GPU_INFO=$(nvidia-smi --query-gpu=name,memory.total --format=csv,noheader 2>/dev/null | head -1)
    if [ -n "$GPU_INFO" ]; then
        print_status "GPU detected: $GPU_INFO"

        # Test NVENC actually works (not just compiled in)
        print_status "Testing NVENC encoding..."
        if ffmpeg -v error -f lavfi -i "color=c=black:s=256x256:d=1:r=25" -pix_fmt yuv420p -c:v h264_nvenc -frames:v 1 -f null - 2>/dev/null; then
            print_status "NVENC encoding works ✓"
            NVENC_OK=true
        else
            print_warning "NVENC test FAILED — GPU encoding will fall back to CPU."
            print_warning ""
            print_warning "This is usually because NVIDIA_DRIVER_CAPABILITIES is not set."
            print_warning "To fix: In RunPod Dashboard → Edit Pod → Environment Variables:"
            print_warning "  NVIDIA_DRIVER_CAPABILITIES=all"
            print_warning "Then restart the pod."
            print_warning ""
        fi
    else
        print_warning "nvidia-smi found but no GPU detected. Will use CPU fallback."
    fi
else
    print_warning "No NVIDIA GPU detected. Will use CPU fallback."
fi

# ==========================================
# Install Node.js
# ==========================================
if command -v node &>/dev/null; then
    print_status "Node.js already installed: $(node --version)"
else
    print_status "Installing Node.js $NODE_VERSION..."
    curl -fsSL "https://deb.nodesource.com/setup_${NODE_VERSION}.x" | bash - 2>/dev/null
    apt-get install -y -qq nodejs 2>/dev/null
    print_status "Node.js installed: $(node --version)"
fi

# ==========================================
# Setup application directory
# ==========================================
print_status "Setting up application directory..."
mkdir -p "$APP_DIR"
mkdir -p "$APP_DIR/scripts"
mkdir -p "$PID_DIR"
mkdir -p "$LOG_DIR"
cd "$APP_DIR"

# ==========================================
# Download binary (if not exists or update)
# ==========================================
ARCH=$(uname -m)
if [ "$ARCH" = "x86_64" ]; then
    BINARY="linux"
elif [ "$ARCH" = "aarch64" ]; then
    BINARY="linux-arm64"
else
    print_error "Unsupported architecture: $ARCH"
    exit 1
fi

if [ ! -f "$APP_DIR/$APP_NAME" ]; then
    print_status "Downloading binary ($BINARY)..."
    curl -fsSL "$URL_BASE/$BINARY" -o "$APP_DIR/$APP_NAME"
    chmod +x "$APP_DIR/$APP_NAME"
    print_status "Binary downloaded."
else
    print_status "Binary already exists. Updating..."
    curl -fsSL "$URL_BASE/$BINARY" -o "$APP_DIR/$APP_NAME.new"
    chmod +x "$APP_DIR/$APP_NAME.new"
    mv "$APP_DIR/$APP_NAME.new" "$APP_DIR/$APP_NAME"
    print_status "Binary updated."
fi

# ==========================================
# Download SCP scripts & install deps
# ==========================================
print_status "Setting up SCP scripts..."
curl -fsSL "$URL_BASE/scripts.tar.gz" | tar xz -C "$APP_DIR" --strip-components=0

cd "$APP_DIR/scripts"
npm install --production --silent 2>/dev/null
cd "$APP_DIR"
print_status "SCP scripts ready."

# ==========================================
# Create/update .env file
# ==========================================
if [ -f "$APP_DIR/.env" ] && [ -z "$MONGODB_URI" ]; then
    print_status "Existing .env found — keeping it."
else
    if [ -z "$MONGODB_URI" ]; then
        print_error "MongoDB URI is required. Use --mongodb-uri \"mongodb+srv://...\""
        exit 1
    fi

    print_status "Creating .env file..."
    cat > "$APP_DIR/.env" <<EOF
MONGODB_URI=$MONGODB_URI
STORAGE_ID=$STORAGE_ID
STORAGE_PATH=$STORAGE_PATH
EOF
    print_status ".env file created."
fi

# ==========================================
# Stop existing workers
# ==========================================
stop_workers

# ==========================================
# Start workers with nohup
# ==========================================
print_status "Starting $WORKER_COUNT worker(s)..."

for i in $(seq 1 $WORKER_COUNT); do
    WORKER_ID="$(hostname)-transcode-$i"
    WORKER_LOG="$LOG_DIR/worker-$i.log"

    # Export env vars and start
    (
        export NVIDIA_DRIVER_CAPABILITIES=all
        export WORKER_ID="$WORKER_ID"
        # Source .env
        set -a
        source "$APP_DIR/.env"
        set +a
        # Override WORKER_ID
        export WORKER_ID="$WORKER_ID"

        nohup "$APP_DIR/$APP_NAME" >> "$WORKER_LOG" 2>&1 &
        echo $! > "$PID_DIR/worker-$i.pid"
    )

    pid=$(cat "$PID_DIR/worker-$i.pid")
    print_status "Worker $i started (PID: $pid, ID: $WORKER_ID)"
    sleep 0.5
done

# ==========================================
# Verify workers
# ==========================================
sleep 2
print_status "Verifying workers..."
RUNNING=0
for i in $(seq 1 $WORKER_COUNT); do
    pidfile="$PID_DIR/worker-$i.pid"
    if [ -f "$pidfile" ]; then
        pid=$(cat "$pidfile")
        if kill -0 "$pid" 2>/dev/null; then
            RUNNING=$((RUNNING + 1))
        else
            print_warning "Worker $i (PID: $pid) is not running!"
            echo "--- Last 10 lines of log ---"
            tail -10 "$LOG_DIR/worker-$i.log" 2>/dev/null || true
        fi
    fi
done

# ==========================================
# Create auto-start script for pod restart
# ==========================================
print_status "Creating auto-start script..."
cat > "$APP_DIR/start.sh" <<'STARTEOF'
#!/bin/bash
# Auto-start script for RunPod pod restart
# Add this to RunPod "Docker Command" or run manually after pod restart

APP_DIR="/workspace/server-transcode"
PID_DIR="$APP_DIR/pids"
LOG_DIR="$APP_DIR/log"

cd "$APP_DIR"

# Count workers from existing pid files or default to 1
WORKER_COUNT=$(ls "$PID_DIR"/*.pid 2>/dev/null | wc -l)
[ "$WORKER_COUNT" -eq 0 ] && WORKER_COUNT=1

# Kill any existing workers
pkill -f "$APP_DIR/server-transcode" 2>/dev/null || true
sleep 1

echo "[$(date)] Starting $WORKER_COUNT worker(s)..."

for i in $(seq 1 $WORKER_COUNT); do
    WORKER_ID="$(hostname)-transcode-$i"
    WORKER_LOG="$LOG_DIR/worker-$i.log"

    (
        export NVIDIA_DRIVER_CAPABILITIES=all
        export WORKER_ID="$WORKER_ID"
        set -a
        source "$APP_DIR/.env"
        set +a
        export WORKER_ID="$WORKER_ID"

        nohup "$APP_DIR/server-transcode" >> "$WORKER_LOG" 2>&1 &
        echo $! > "$PID_DIR/worker-$i.pid"
    )

    pid=$(cat "$PID_DIR/worker-$i.pid")
    echo "[$(date)] Worker $i started (PID: $pid)"
    sleep 0.5
done

echo "[$(date)] All workers started."
STARTEOF
chmod +x "$APP_DIR/start.sh"

# Create stop script
cat > "$APP_DIR/stop.sh" <<'STOPEOF'
#!/bin/bash
# Stop all workers
APP_DIR="/workspace/server-transcode"
PID_DIR="$APP_DIR/pids"

echo "Stopping all workers..."
for pidfile in "$PID_DIR"/*.pid; do
    [ -f "$pidfile" ] || continue
    pid=$(cat "$pidfile")
    name=$(basename "$pidfile" .pid)
    if kill -0 "$pid" 2>/dev/null; then
        kill "$pid" 2>/dev/null
        echo "Stopped $name (PID: $pid)"
    fi
    rm -f "$pidfile"
done
pkill -f "$APP_DIR/server-transcode" 2>/dev/null || true
echo "Done."
STOPEOF
chmod +x "$APP_DIR/stop.sh"

# Create uninstall script
cat > "$APP_DIR/uninstall.sh" <<'UNINSTALLEOF'
#!/bin/bash
# Uninstall server-transcode completely
APP_DIR="/workspace/server-transcode"
PID_DIR="$APP_DIR/pids"

echo "🗑️  Uninstalling Server Transcode..."
echo ""

# Stop workers
echo "[1/5] Stopping all workers..."
for pidfile in "$PID_DIR"/*.pid; do
    [ -f "$pidfile" ] || continue
    pid=$(cat "$pidfile")
    name=$(basename "$pidfile" .pid)
    if kill -0 "$pid" 2>/dev/null; then
        kill "$pid" 2>/dev/null
        echo "  Stopped $name (PID: $pid)"
    fi
    rm -f "$pidfile"
done
pkill -f "$APP_DIR/server-transcode" 2>/dev/null || true
echo "  All workers stopped."

# Remove FFmpeg
echo "[2/5] Removing FFmpeg..."
if [ -f "/usr/local/bin/ffmpeg" ]; then
    rm -f /usr/local/bin/ffmpeg /usr/local/bin/ffprobe
    echo "  Removed FFmpeg."
else
    echo "  FFmpeg not found — skipping."
fi

# Remove Node.js
echo "[3/5] Removing Node.js..."
if command -v node &>/dev/null; then
    apt-get remove -y -qq nodejs 2>/dev/null || true
    apt-get autoremove -y -qq 2>/dev/null || true
    rm -f /etc/apt/sources.list.d/nodesource.list 2>/dev/null || true
    rm -f /etc/apt/keyrings/nodesource.gpg 2>/dev/null || true
    echo "  Node.js removed."
else
    echo "  Node.js not found — skipping."
fi

# Clean temp
echo "[4/5] Cleaning temp files..."
rm -rf /tmp/ffmpeg-build 2>/dev/null || true
echo "  Done."

# Remove app directory (do this last since we're running from it)
echo "[5/5] Removing application directory..."
cd /workspace
rm -rf "$APP_DIR"
echo "  Removed $APP_DIR"

echo ""
echo "✅ Uninstallation completed!"
UNINSTALLEOF
chmod +x "$APP_DIR/uninstall.sh"

# ==========================================
# Summary
# ==========================================
echo ""
print_header "============================================"
if [ $RUNNING -eq $WORKER_COUNT ]; then
    print_header "  ✅ Installation completed successfully!"
else
    print_header "  ⚠️  Installation completed ($RUNNING/$WORKER_COUNT running)"
fi
print_header "============================================"
echo ""
echo "  Directory:  $APP_DIR"
echo "  Workers:    $RUNNING / $WORKER_COUNT running"
echo "  GPU:        ${GPU_INFO:-Not detected}"
echo "  NVENC:      $([ "$NVENC_OK" = true ] && echo '✅ Working' || echo '❌ Not working (CPU fallback)')"
echo "  FFmpeg:     $(ffmpeg -version 2>/dev/null | head -1 | cut -d' ' -f1-3)"
echo "  Node.js:    $(node --version)"
echo ""
echo "  Commands:"
echo "    View logs:      tail -f $LOG_DIR/worker-1.log"
echo "    View all logs:  tail -f $LOG_DIR/worker-*.log"
echo "    Stop workers:   $APP_DIR/stop.sh"
echo "    Start workers:  $APP_DIR/start.sh"
echo "    Restart:        $APP_DIR/stop.sh && $APP_DIR/start.sh"
echo "    Uninstall:      $APP_DIR/uninstall.sh"
echo ""
echo "  Pod restart:"
echo "    Workers auto-start → set RunPod start command to:"
echo "    bash /workspace/server-transcode/start.sh"
echo ""
print_header "============================================"
