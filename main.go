package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	gracefulShutdownTimeout = 30 * time.Second
	maxGoroutines           = 100
	healthCheckInterval     = 5 * time.Minute
	memoryReportInterval    = 10 * time.Minute
	apiHeartbeatInterval    = 30 * time.Second
)

var (
	goroutineCounter int64
	lastHealthCheck  time.Time
	pidFileName      = "squad_rcon_client.pid"
)


func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func killProcess(pid int, force bool) error {
	if pid <= 0 {
		return fmt.Errorf("无效的PID: %d", pid)
	}
	var cmd *exec.Cmd
	if force {
		cmd = exec.Command("kill", "-9", strconv.Itoa(pid))
	} else {
		cmd = exec.Command("kill", "-15", strconv.Itoa(pid))
	}
	return cmd.Run()
}

func checkAndCleanupOldProcesses() error {
	LogInfo("检查是否有旧的进程实例...")

	if _, err := os.Stat(pidFileName); err == nil {
		content, err := ioutil.ReadFile(pidFileName)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(content)), "\n")
			if len(lines) >= 2 {
				if pid, err := strconv.Atoi(lines[0]); err == nil && pid > 0 {
					startTime := "未知"
					if len(lines) > 1 {
						startTime = lines[1]
					}

					if isProcessRunning(pid) {
						LogWarn("发现运行中的旧进程: PID=%d, 启动时间=%s", pid, startTime)
						LogInfo("正在终止旧进程...")

						if err := killProcess(pid, false); err != nil {
							LogWarn("优雅终止失败: %v", err)
						} else {
							for i := 0; i < 10; i++ {
								time.Sleep(1 * time.Second)
								if !isProcessRunning(pid) {
									LogInfo("旧进程已优雅退出")
									break
								}
							}
						}

						if isProcessRunning(pid) {
							LogWarn("强制终止旧进程...")
							if err := killProcess(pid, true); err != nil {
								LogError("强制终止失败: %v", err)
							}
						}
					}
				}
			}
		}
		os.Remove(pidFileName)
	}

	currentPID := os.Getpid()
	startTime := time.Now().Format("2006-01-02 15:04:05")
	pidContent := fmt.Sprintf("%d\n%s\n", currentPID, startTime)
	ioutil.WriteFile(pidFileName, []byte(pidContent), 0644)
	LogInfo("已写入PID文件: %s (PID: %d)", pidFileName, currentPID)

	return nil
}

func cleanupPIDFile() {
	if err := os.Remove(pidFileName); err != nil {
		LogWarn("删除PID文件失败: %v", err)
	} else {
		LogInfo("已删除PID文件: %s", pidFileName)
	}
}


func SafeGoroutine(name string, fn func(), wg *sync.WaitGroup) {
	current := atomic.AddInt64(&goroutineCounter, 1)
	if current > maxGoroutines {
		atomic.AddInt64(&goroutineCounter, -1)
		LogError("Goroutine数量超过限制 (%d)，拒绝启动: %s", maxGoroutines, name)
		if wg != nil {
			wg.Done()
		}
		return
	}

	if wg != nil {
		wg.Add(1)
	}

	go func() {
		defer func() {
			atomic.AddInt64(&goroutineCounter, -1)
			if wg != nil {
				wg.Done()
			}
			if r := recover(); r != nil {
				LogError("Goroutine '%s' 发生panic: %v", name, r)
				LogError("堆栈信息:\n%s", debug.Stack())
				timestamp := time.Now().Format("2006-01-02_15-04-05")
				filename := fmt.Sprintf("panic_%s_%s.log", name, timestamp)
				panicInfo := fmt.Sprintf("时间: %s\nGoroutine: %s\nPanic: %v\n堆栈:\n%s\n",
					time.Now().Format("2006-01-02 15:04:05"), name, r, debug.Stack())
				ioutil.WriteFile(filename, []byte(panicInfo), 0644)
			}
		}()
		LogDebug("Goroutine '%s' 已启动", name)
		fn()
		LogDebug("Goroutine '%s' 正常退出", name)
	}()
}


func getMemoryUsage() (alloc, sys uint64) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Alloc, m.Sys
}

func performHealthCheck(dbManager *DBManager, playerMonitor *PlayerMonitor, combatLogMonitor *CombatLogMonitor) {
	lastHealthCheck = time.Now()
	alloc, sys := getMemoryUsage()
	goroutines := runtime.NumGoroutine()

	LogInfo("=== 健康检查报告 ===")
	LogInfo("  Goroutines: %d", goroutines)
	LogInfo("  内存: 分配=%.2fMB, 系统=%.2fMB", float64(alloc)/1024/1024, float64(sys)/1024/1024)

	if dbManager != nil {
		dbManager.LogConnectionStats()
		if onlineCount, err := dbManager.GetOnlinePlayersCount(); err == nil {
			LogInfo("  在线玩家数: %d", onlineCount)
		}
	}

	if playerMonitor != nil {
		LogInfo("  玩家监控器: %v", playerMonitor.GetConnectionStatus())
	}
	if combatLogMonitor != nil {
		LogInfo("  战斗日志监控器: 对局ID=%s, 玩家缓存=%d",
			combatLogMonitor.GetCurrentMatchID(), combatLogMonitor.GetPlayerCount())

		if tpsStats, err := combatLogMonitor.GetTPSStats(); err == nil {
			if latestTPS, ok := tpsStats["latest_tps"].(float64); ok && latestTPS >= 0 {
				LogInfo("  最新TPS: %.2f", latestTPS)
			}
		}
	}
}


func main() {
	fmt.Printf("SquadRank排位系统日志获取器 v%s\n", VERSION)
	fmt.Println("===========================================")

	localConfig, err := LoadLocalConfig()
	if err != nil {
		LogError("加载本地配置失败: %v", err)
		fmt.Println("\n按回车键退出...")
		fmt.Scanln()
		return
	}

	if err := checkAndCleanupOldProcesses(); err != nil {
		LogError("检查旧进程失败: %v", err)
	}
	defer cleanupPIDFile()

	cfg, err := FetchConfigFromAPI(localConfig.UniqueID)
	if err != nil {
		LogError("从API获取配置失败: %v", err)
		fmt.Println("\n按回车键退出...")
		fmt.Scanln()
		return
	}

	cfg.RCONHost = localConfig.RCON.Host
	cfg.RCONPort = localConfig.RCON.Port
	cfg.RCONPassword = localConfig.RCON.Password

	SetLogLevel(cfg.LogLevel)
	LogInfo("程序启动 - PID: %d, 版本: v%s", os.Getpid(), VERSION)
	LogInfo("Go版本: %s, OS: %s, 架构: %s", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	mainCtx, mainCancel := context.WithCancel(context.Background())
	defer mainCancel()

	if err := RegisterConnection(cfg.UniqueID); err != nil {
		LogWarn("注册连接失败: %v", err)
	}

	defer func() {
		if err := DisconnectFromAPI(cfg.UniqueID); err != nil {
			LogWarn("断开连接通知失败: %v", err)
		}
	}()

	dbManager, dbErr := NewDBManager(cfg)
	if dbErr != nil {
		LogError("初始化数据库管理器失败: %v", dbErr)
		LogInfo("提示: 数据库连接失败，程序将退出。")
		fmt.Println("\n按回车键退出...")
		fmt.Scanln()
		return
	}
	defer func() {
		LogInfo("正在关闭数据库连接...")
		dbManager.Close()
	}()

	osSignal := make(chan os.Signal, 1)
	signal.Notify(osSignal, syscall.SIGINT, syscall.SIGTERM)

	var mainWg sync.WaitGroup


	var playerMonitor *PlayerMonitor
	playerMonitor = NewPlayerMonitor(cfg, dbManager)
	if err := dbManager.ClearAllOnlinePlayers(); err != nil {
		LogWarn("清空在线玩家表失败: %v", err)
	}
	if err := playerMonitor.Start(); err != nil {
		LogError("启动玩家监控器失败: %v", err)
		playerMonitor = nil
	} else {
		SafeGoroutine("PlayerMonitor-Stopper", func() {
			<-mainCtx.Done()
			LogInfo("正在停止玩家监控器...")
			playerMonitor.Stop()
		}, &mainWg)
	}

	var combatLogMonitor *CombatLogMonitor
	combatLogMonitor = NewCombatLogMonitor(cfg, dbManager)
	if err := combatLogMonitor.Start(); err != nil {
		LogError("启动战斗日志监控器失败: %v", err)
		combatLogMonitor = nil
	} else {
		SafeGoroutine("CombatLogMonitor-Stopper", func() {
			<-mainCtx.Done()
			LogInfo("正在停止战斗日志监控器...")
			combatLogMonitor.Stop()
		}, &mainWg)
	}

	SafeGoroutine("Signal-Handler", func() {
		sig := <-osSignal
		LogInfo("收到操作系统信号: %v，正在关闭...", sig)
		mainCancel()
	}, &mainWg)

	SafeGoroutine("API-Heartbeat", func() {
		heartbeatTicker := time.NewTicker(apiHeartbeatInterval)
		defer heartbeatTicker.Stop()
		for {
			select {
			case <-mainCtx.Done():
				return
			case <-heartbeatTicker.C:
				if err := SendHeartbeat(cfg.UniqueID); err != nil {
					LogWarn("API心跳发送失败: %v", err)
				} else {
					LogDebug("API心跳已发送")
				}
			}
		}
	}, &mainWg)

	SafeGoroutine("Health-Monitor", func() {
		healthTicker := time.NewTicker(healthCheckInterval)
		defer healthTicker.Stop()
		performHealthCheck(dbManager, playerMonitor, combatLogMonitor)
		for {
			select {
			case <-mainCtx.Done():
				return
			case <-healthTicker.C:
				performHealthCheck(dbManager, playerMonitor, combatLogMonitor)
			}
		}
	}, &mainWg)

	SafeGoroutine("Admin-Flight-Monitor", func() {
		flightTicker := time.NewTicker(1 * time.Minute)
		defer flightTicker.Stop()
		LogInfo("飞天监控器已启动")
		for {
			select {
			case <-mainCtx.Done():
				return
			case <-flightTicker.C:
				if err := dbManager.CheckAndCloseOfflineAdminFlights(); err != nil {
					LogError("飞天监控器检查失败: %v", err)
				}
			}
		}
	}, &mainWg)

	if combatLogMonitor != nil {
		SafeGoroutine("TPS-Cleanup-Monitor", func() {
			tpsTicker := time.NewTicker(24 * time.Hour)
			defer tpsTicker.Stop()
			for {
				select {
				case <-mainCtx.Done():
					return
				case <-tpsTicker.C:
					if err := combatLogMonitor.ForceCleanTPSLogs(); err != nil {
						LogError("TPS数据清理失败: %v", err)
					}
				}
			}
		}, &mainWg)
	}

	SafeGoroutine("Memory-Reporter", func() {
		memoryTicker := time.NewTicker(memoryReportInterval)
		defer memoryTicker.Stop()
		for {
			select {
			case <-mainCtx.Done():
				return
			case <-memoryTicker.C:
				alloc, sys := getMemoryUsage()
				LogInfo("内存报告 - 分配: %.2fMB, 系统: %.2fMB, Goroutines: %d",
					float64(alloc)/1024/1024, float64(sys)/1024/1024, runtime.NumGoroutine())
				if alloc > 100*1024*1024 {
					runtime.GC()
					debug.FreeOSMemory()
				}
			}
		}
	}, &mainWg)

	LogInfo("所有监控器已启动，等待退出信号...")
	LogInfo("功能: 在线玩家监控 | 击杀/击倒/救援日志 | TPS记录 | API心跳")

	<-mainCtx.Done()

	LogInfo("正在优雅关闭程序...")

	done := make(chan struct{})
	go func() {
		mainWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		LogInfo("所有后台goroutine已安全退出")
	case <-time.After(gracefulShutdownTimeout):
		LogWarn("等待后台goroutine退出超时，强制结束程序")
	}

	LogInfo("日志监控客户端已退出。")
}
