BIN := bin/claudego
PREFIX ?= /opt/homebrew/bin

build:
	go build -o $(BIN) .

test: build
	bash test/integration.sh

vet:
	go vet ./...

install: build
	# 先删再拷（新 inode）：macOS 上 cp 原位覆盖已签名二进制会让签名缓存失效，
	# 新起的进程被 AMFI 直接 SIGKILL（RC=137）。正在运行的旧映像不受影响。
	rm -f $(PREFIX)/claudego
	cp $(BIN) $(PREFIX)/claudego
	@echo "已安装到 $(PREFIX)/claudego（launchd 请在安装后重新运行 claudego install-launchd）"

# 【CG-R2 R2·P1-3】仓内验收入口:硬编码 CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 下跑同步/指纹用例。
# 缺省 go test 在未装机机器上会 Skip → 套件绿但对护栏部署状态零证明力(上一轮 P1 静默漂绿)。
# 装机验收流水/收工汇报请引用 `make accept-sync` 的实跑输出而非 `go test ./...` 的绿。
accept-sync:
	@echo "==> accept-sync: CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 go test -run 'Sync|Verify' -count=1 -v"
	CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 go test -run 'Sync|Verify' -count=1 -v ./...

.PHONY: build test vet install accept-sync
