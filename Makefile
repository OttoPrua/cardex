BIN := bin/cardex
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
	rm -f $(PREFIX)/cardex
	cp $(BIN) $(PREFIX)/cardex
	@echo "已安装到 $(PREFIX)/cardex（launchd 请在安装后重新运行 cardex install-launchd）"

# install-shim: 旧名 claudego → 新名 cardex 的兼容软链（BD-44 改名过渡期）。
# 【为什么独立成目标而不塞进 install】存量脚本/别名/人肌肉记忆还打 `claudego`,直接断名会让
# 一堆外部调用点静默 command not found;但 shim 是**过渡期产物**,该由 cutover 操作员显式决定
# 何时铺、何时撤,不能让每次 `make install` 都无声重建它(撤掉后下一次 install 又长回来)。
# 用 ln -sf 而非 cp:软链跟随 cardex 本体升级,不会留下一个永远停在旧版本的二进制副本。
install-shim: install
	ln -sf $(PREFIX)/cardex $(PREFIX)/claudego
	@echo "已建兼容软链 $(PREFIX)/claudego -> $(PREFIX)/cardex（过渡期用;改名收尾后可 rm 掉）"

# 【CG-R2 R2·P1-3】仓内验收入口:硬编码 CARDEX_REQUIRE_SYNC_SCRIPTS=1 下跑同步/指纹用例。
# 缺省 go test 在未装机机器上会 Skip → 套件绿但对护栏部署状态零证明力(上一轮 P1 静默漂绿)。
# 装机验收流水/收工汇报请引用 `make accept-sync` 的实跑输出而非 `go test ./...` 的绿。
# 【CG-R2b R2·2026-07-24】pattern 追加 DesignReview,覆盖 TestDesignReviewAndFixCycleTemplatesEmbedContractContent
# 的 (c) 段装机副本断言;pattern 覆盖度由 TestAcceptSyncMakefilePatternCoversAllEnvGatedTests 动态自校。
accept-sync:
	@echo "==> accept-sync: CARDEX_REQUIRE_SYNC_SCRIPTS=1 go test -run 'Sync|Verify|DesignReview' -count=1 -v"
	CARDEX_REQUIRE_SYNC_SCRIPTS=1 go test -run 'Sync|Verify|DesignReview' -count=1 -v ./...

.PHONY: build test vet install install-shim accept-sync
