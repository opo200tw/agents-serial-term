# serial-term

Cross-platform serial debug terminal. Single Go binary, no runtime deps.

Designed to replace ad-hoc `screen` / `minicom` / PuTTY / IDE serial monitors during embedded bring-up, and to produce output that both humans and automation (incl. LLMs) can read.

## 常用指令（快速查詢）

> **Port path 不是定值**，會因每台機器 / 每顆晶片 / 插的 USB hub 位置而變：
>
> - macOS FTDI：`/dev/cu.usbserial-<晶片序號>`，例如 `/dev/cu.usbserial-A50285BI`
> - macOS CDC：`/dev/cu.usbmodem<enum>`，例如 `/dev/cu.usbmodem1101`
> - Linux FTDI：`/dev/ttyUSB<n>`；Linux CDC：`/dev/ttyACM<n>`
> - Windows：`COM<n>`
>
> 先查路徑再開，建議用 shell 變數省事：
>
> ```bash
> ./serial-term --list-ports                              # 先看有哪些
> PORT=$(./serial-term --list-ports | grep usbserial | head -1)   # macOS FTDI
> # 或 ttyUSB / usbmodem / 自己挑一個貼路徑進去
> ```
>
> 下面範例一律用 `"$PORT"`。不想動 shell 也可以直接 `./serial-term`（無參數）讓工具自己選 / 出選單。

### 啟動

```bash
# 編譯
make

# 列出可用 port（不開啟）
./serial-term --list-ports

# 自動選：單一 port 直接開、多 port 出選單
./serial-term

# 指定 port（$PORT 請先從 --list-ports 選一個 — 見下方 note）
./serial-term "$PORT"

# 指定 baud
./serial-term --baud 921600 "$PORT"

# 帶 .txt log 長時間監聽（永遠不退，Ctrl-C 停）
./serial-term --text-log /tmp/uart.txt "$PORT" < /dev/null

# 同時存 binary raw + 帶時間戳 text
./serial-term --log raw.bin --text-log session.txt "$PORT"

# 不同初始模式
./serial-term --mode ascii  "$PORT"
./serial-term --mode hex    "$PORT"
./serial-term --mode mixed  "$PORT"    # 預設

# EOL 變體
./serial-term --tx-eol crlf "$PORT"    # Windows / Modbus ASCII
./serial-term --tx-eol none "$PORT"    # binary 協定
```

### UI 內 slash commands（底部 input 打完按 Enter）

```
任何字串              送到裝置 + tx-eol（例：PING → 裝置收到 "PING\n"）

/hex  /h             切 RX 顯示 = 16 bytes/line hex
/ascii  /a           切 ASCII + <CR>/<LF>/<CRLF> 標記
/mixed  /m           切 hexdump -C（預設）
/ts  /t              切時間戳開關（每行前面加 HH:MM:SS.mmm）

/eol                 看目前 TX EOL
/eol lf              改 EOL 為 \n
/eol crlf            改 EOL 為 \r\n
/eol cr              改 EOL 為 \r
/eol none            不附加 EOL

/hex-send 47 45 54 0a       送 raw bytes（= "GET\n"），不附 EOL
/hex-send DEADBEEF          白空格無所謂、大小寫皆可

/bench                      loopback 吞吐測試：5s、chunk=4096（預設）
/bench 10                   改 10 秒
/bench 10 8192              10 秒、chunk 8 KB
/bench-stop                 中斷

/help  /?            列這張表在 sys 面板
/quit  /q  /exit     退出（Ctrl-C 也可以）
```

### 腳本用法（非互動 / pipe 進 stdin）

```bash
# 單發一個命令、等回、退出
(printf 'PING\n'; sleep 0.5; printf '/quit\n') \
  | ./serial-term "$PORT"

# 背景跑、只收 log 不互動
./serial-term --text-log /tmp/uart.txt "$PORT" \
  < /dev/null > /dev/null 2>&1 &

# CI / smoke test：送幾個 PING、看有沒有 PONG
(for i in 1 2 3; do printf 'PING\n'; sleep 0.3; done; printf '/quit\n') \
  | ./serial-term "$PORT" | grep -c PONG
```

### Loopback 吞吐測試（TX↔RX 短接）

```bash
./serial-term --baud 3000000 "$PORT"
# 開啟後在 input 打：
/bench 5
# sys 面板 ~5 秒後出現：
# --- bench: end  tx=611301  rx=611301  mm=0  t=5.02s  rx=0.231 MB/s ---
```

實測 FT232R 上限（TX↔RX 短接 loopback）：

| baud | MB/s | 效率 |
|------|------|------|
| 115200 | 0.009 | 85% |
| 921600 | 0.073 | 83% |
| 1 Mbaud | 0.079 | 82% |
| 3 Mbaud | 0.231 | 81% |

### 解 `--log` 落檔（事後離線分析）

```bash
# 直接看 hex
xxd /tmp/raw.bin | less

# Python 解
python3 <<'EOF'
import sys
data = open('/tmp/raw.bin', 'rb').read()
print(f'{len(data)} bytes, {data[:60]!r}...')
EOF
```

---

## Features

- **TUI by default** when stdin & stdout are a terminal: split panes for RX / TX / system log / options + status bar showing port / mode / tx-eol / log paths + bottom input field. Falls back to line-buffered headless mode in pipes / scripts / background.
- **Bidirectional**. RX is displayed with a `<` prefix, TX with `>`. Same stream, same format.
- **Three display modes** for RX:
  - **ascii** — printable as-is, non-printable as `\xNN` / `\t` escapes, CR / LF / CRLF each get their own marker at end of line (`<CR>` / `<LF>` / `<CRLF>`), so the device's actual line ending is visible.
  - **hex** — 16 bytes per line, space-separated.
  - **mixed** — classic `hexdump -C` layout (offset + hex + ASCII side column), tracks a running global offset so captures are replayable.
- **EOL handling**: all three endings (CR, LF, CRLF) flush the current display line; CRLF is detected as a pair so you never see it printed as two empty lines.
- **TX with configurable EOL**: `--tx-eol none|cr|lf|crlf`. Each line typed on stdin is sent with that ending appended.
- **Raw byte TX**: `/hex-send 47 45 54 0a` for non-ASCII probing.
- **Line-buffered stdin** works the same whether you're typing interactively, piping a script, or running in the background. No raw-mode keyboard voodoo — just type a line, press Enter.
- **Local commands** start with `/` so device traffic and tool commands never collide.
- `--log FILE` byte-perfect raw RX capture (binary), independent of display mode.
- `--text-log FILE` mirrors the rendered display to a text file, with full date+time stamp on every line. Writes go through the OS page cache — `tail -f` sees them immediately — but `fsync` only happens on clean shutdown (too expensive per-line at 300 KB/s). A crash can lose up to a few seconds of trailing log.
- `--ts` adds short `HH:MM:SS.mmm ` prefix to **stdout** (text-log always carries the full date regardless).
- Port picker when PORT is omitted: 0 ports → error; 1 port → auto-select; multiple ports → numbered list + stdin prompt. `--list-ports` just prints the list and exits (good for scripting). macOS `/dev/tty.<X>` duplicates of `/dev/cu.<X>` are filtered out automatically.
- Cross-platform (linux / darwin / windows × amd64 / arm64) via `go.bug.st/serial`.

## Build

```
make            # native → serial-term
make dist       # cross-compile matrix → dist/
```

Or plain `go build .`.

## Usage

```
serial-term [flags] [PORT]

flags:
  --baud N        baud rate (default 115200)
  --mode M        initial display: ascii | hex | mixed (default mixed)
  --log FILE      append raw received bytes (byte-for-byte, binary)
  --text-log FILE append rendered display lines with full date+time stamp
                  (text, ideal for tail -f / grep / archival)
  --ts            start stdout with HH:MM:SS.mmm prefix on each line
  --tx-eol E      append to each TX line: none | cr | lf | crlf (default lf)

interactive commands on stdin (type then Enter):
  /hex | /h              switch display to hex
  /ascii | /a            switch display to ASCII with EOL markers
  /mixed | /m            switch display to hexdump -C style
  /ts | /t               toggle timestamps
  /eol [MODE]            get/set TX EOL (none/cr/lf/crlf)
  /hex-send HEX...       send raw bytes (no EOL), e.g. /hex-send 01 02 ff
  /quit | /q             exit
  any other line         sent to the device + tx-eol

PORT:
  macOS    /dev/cu.usbmodem*     CDC-ACM (Arduino, bare USB devices)
           /dev/cu.usbserial-*   FTDI FT232 / CP210x / CH34x / ...
  Linux    /dev/ttyACM<n>        CDC-ACM
           /dev/ttyUSB<n>        FTDI / CP210x / CH34x / PL2303
  Windows  COM<n>                all
  If omitted the tool lists all detected ports and prompts you to pick one
  (uses go.bug.st/serial's GetPortsList — covers everything the OS reports).
  --list-ports just prints the list and exits.
```

## Interactive session example

(Showing a sample CDC device at `/dev/cu.usbmodem<N>` — your actual port path is whatever `--list-ports` reports.)

```
$ serial-term /dev/cu.usbmodem1101
serial-term: /dev/cu.usbmodem1101 @ 115200  mode=mixed  tx-eol=lf  (/help for cmds)
PING
> PING<LF>
< 00000000  48 52 53 50 00 00 00 00  00 00 00 00 00 00 00 00  |HRSP............|
< 00000010  04 00 00 00 50 4f 4e 47  78 a6 39 e9              |....PONGx.9.|
/a
--- mode=ascii ---
VERSION
> VERSION<LF>
< HRSP\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x0e\x00\x00\x00htpa_cdc 0.3.0\x80\xecfv
/quit
```

## Script example

```
printf 'PING\nSTATUS\nGET_FRAME\n/quit\n' | \
  serial-term --mode mixed --log /tmp/capture.bin "$PORT"
```

`/tmp/capture.bin` will contain byte-for-byte everything the device sent back. `/quit` terminates cleanly after the replies arrive.

## TUI layout

```
┌─ status: port=...  mode=...  tx-eol=...  ts=...  --log=...  --text-log=... ─┐
│ ┌──── RX (device → host) ─────────────────────────┐ ┌── TX (host → device) ──┐
│ │ < 00000000  48 52 53 50 ...  |HRSP....|         │ │ > PING<LF>             │
│ │ < 00000010  04 00 00 00 ...  |....PONG..|       │ │ > VERSION<LF>          │
│ │                                                 │ │                        │
│ │                                                 │ ├── log / system ────────┤
│ │                                                 │ │ --- session start ---  │
│ │                                                 │ │ --- text-log: /tmp/... │
│ │                                                 │ │ --- mode=ascii ---     │
│ │                                                 │ ├── options ─────────────┤
│ │                                                 │ │ display modes /hex /a..│
│ │                                                 │ │ ts            /ts      │
│ │                                                 │ │ tx EOL        /eol ... │
│ │                                                 │ │ help          /help    │
│ │                                                 │ │ quit          /quit    │
│ └─────────────────────────────────────────────────┘ └────────────────────────┘
│ > _                                                                          │
└──────────────────────────────────────────────────────────────────────────────┘
```

Status bar updates live as commands change mode / tx-eol / ts. Both `--log` and `--text-log` paths are visible there too so you always know where the captures land.

## Long-running passive capture

Open the port, log everything to a timestamped text file, run forever, no input needed:

```
serial-term --text-log /var/log/serial-$(date +%Y%m%d).txt "$PORT" < /dev/null
```

`< /dev/null` closes stdin so the tool stops watching for `/commands` and just sits as a passive RX listener. `tail -f` sees new lines immediately (page cache). A crash may lose the last few seconds of buffered writes; clean shutdown via Ctrl-C / SIGTERM `fsync`s the file and writes the session-end summary.

Sample `.txt` content:

```
2026-04-24 12:28:38.474 --- session start: /dev/cu.usbmodem1101 @ 115200, mode=mixed, tx-eol=lf ---
2026-04-24 12:28:38.486 < 00000000  48 52 53 50 14 00 00 00  00 00 00 00 00 00 00 00  |HRSP............|
2026-04-24 12:28:38.491 < 00000010  04 00 00 00 50 4f 4e 47  04 e6 12 47              |....PONG...G|
```

## Why not `screen` / `minicom`?

- **Cross-platform single binary**. No `screen` on Windows, `minicom` also Unix-only, PuTTY can't be scripted easily.
- **Unambiguous EOL display**. `screen` hides whether the device sent `\r` or `\n`. `serial-term` prints explicit markers — essential when debugging CDC firmware where `\r\n` vs `\n` changes over time.
- **Hexdump mode built in**. `screen -l /tmp/foo` + `xxd` works but is clumsy and misses real-time view.
- **Machine-parseable output**. Every non-continuation line starts with a stable prefix (timestamp optional) and a direction character so downstream grep / LLM parsing is straightforward.
- **Same tool for interactive + scripted**. Pipe a command sequence in, capture output, done. No separate `expect` / `chat` layer.
