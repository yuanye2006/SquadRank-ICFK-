package main

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)


type PlayerMonitor struct {
	cfg                *Config
	dbManager          *DBManager
	onlinePlayers      map[int]*PlayerInfo
	onlinePlayersMutex sync.RWMutex
	conn               net.Conn
	isRunning          bool
	stopChan           chan struct{}
	serverID           int
	reconnectAttempts  int
}

var (
	onlinePlayerRegex  = regexp.MustCompile(`ID:\s*(\d+)\s*\|\s*Online IDs:\s*EOS:\s*([a-f0-9]*)\s*steam:\s*(\d+)\s*\|\s*Name:\s*(.*?)\s*\|\s*Team ID:\s*(\d+)\s*\|\s*Squad ID:\s*(N/A|\d+)\s*\|\s*Is Leader:\s*(True|False)\s*\|\s*Role:\s*(.*)`)
	offlinePlayerRegex = regexp.MustCompile(`ID:\s*(\d+)\s*\|\s*Online IDs:\s*EOS:\s*([a-f0-9]*)\s*steam:\s*(\d+)\s*\|\s*Since Disconnect:\s*.*?\s*\|\s*Name:\s*(.*)`)
)

func NewPlayerMonitor(cfg *Config, dbManager *DBManager) *PlayerMonitor {
	return &PlayerMonitor{
		cfg:           cfg,
		dbManager:     dbManager,
		onlinePlayers: make(map[int]*PlayerInfo, 1000),
		stopChan:      make(chan struct{}),
		serverID:      cfg.ServerID,
	}
}

func (pm *PlayerMonitor) Start() error {
	LogInfo("正在启动玩家监控器...")
	if err := pm.connectRCON(); err != nil {
		return fmt.Errorf("玩家监控器连接RCON失败: %w", err)
	}
	pm.isRunning = true
	pm.reconnectAttempts = 0
	go pm.monitorLoop()
	LogInfo("玩家监控器已启动")
	return nil
}

func (pm *PlayerMonitor) Stop() {
	if !pm.isRunning {
		return
	}
	pm.isRunning = false
	close(pm.stopChan)
	if pm.conn != nil {
		pm.conn.Close()
	}
	LogInfo("玩家监控器已停止")
}

func (pm *PlayerMonitor) connectRCON() error {
	address := fmt.Sprintf("%s:%d", pm.cfg.RCONHost, pm.cfg.RCONPort)
	LogInfo("玩家监控器正在连接...")
	
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return fmt.Errorf("RCON连接失败")
	}
	if err := Authenticate(conn, pm.cfg.RCONPassword, -100); err != nil {
		conn.Close()
		return fmt.Errorf("RCON认证失败")
	}
	pm.conn = conn
	LogInfo("玩家监控器RCON连接成功")
	return nil
}

func (pm *PlayerMonitor) monitorLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-pm.stopChan:
			return
		case <-ticker.C:
			if err := pm.updatePlayerList(); err != nil {
				LogError("更新玩家列表失败: %v", err)
				if err := pm.reconnectRCON(); err != nil {
					LogError("玩家监控器重连失败: %v", err)
				}
			}
		}
	}
}

func (pm *PlayerMonitor) reconnectRCON() error {
	if pm.conn != nil {
		pm.conn.Close()
		pm.conn = nil
	}
	
	pm.reconnectAttempts++
	
	delay := time.Duration(pm.reconnectAttempts) * 2 * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	
	LogWarn("玩家监控器将在 %v 后重新连接 (尝试次数: %d)...", delay, pm.reconnectAttempts)
	
	select {
	case <-pm.stopChan:
		return fmt.Errorf("监控器已停止")
	case <-time.After(delay):
	}
	
	if err := pm.connectRCON(); err != nil {
		return err
	}
	
	pm.reconnectAttempts = 0
	return nil
}

func (pm *PlayerMonitor) updatePlayerList() error {
	if pm.conn == nil {
		return fmt.Errorf("RCON连接未建立")
	}
	
	if err := SendCommand(pm.conn, "ListPlayers"); err != nil {
		return err
	}
	response, err := pm.receiveResponse()
	if err != nil {
		return err
	}
	currentPlayers, offlinePlayers := pm.parseResponse(response)
	return pm.updateDatabase(currentPlayers, offlinePlayers)
}

func (pm *PlayerMonitor) receiveResponse() (string, error) {
	var responseBuilder strings.Builder
	for {
		pm.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		packet, err := readPacket(pm.conn)
		pm.conn.SetReadDeadline(time.Time{})
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				break
			}
			return "", err
		}
		if packet.Type == ServerDataResponseValue {
			responseBuilder.WriteString(packet.Body)
		}
	}
	return responseBuilder.String(), nil
}

func (pm *PlayerMonitor) parseResponse(response string) (map[int]*PlayerInfo, []*OfflinePlayerInfo) {
	currentPlayers := make(map[int]*PlayerInfo)
	var offlinePlayers []*OfflinePlayerInfo

	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if matches := onlinePlayerRegex.FindStringSubmatch(line); len(matches) == 9 {
			gameID, _ := strconv.Atoi(matches[1])
			steamID, _ := strconv.ParseInt(matches[3], 10, 64)
			factionID, _ := strconv.Atoi(matches[5])
			var squadID *int
			if matches[6] != "N/A" {
				if sid, err := strconv.Atoi(matches[6]); err == nil {
					squadID = &sid
				}
			}
			currentPlayers[gameID] = &PlayerInfo{
				GamePlayerID:   gameID,
				SteamID:        steamID,
				EosID:          strings.TrimSpace(matches[2]),
				PlayerNickname: strings.TrimSpace(matches[4]),
				FactionID:      factionID,
				SquadID:        squadID,
				IsSquadLeader:  strings.TrimSpace(matches[7]),
				PlayerRole:     strings.TrimSpace(matches[8]),
			}
			continue
		}

		if matches := offlinePlayerRegex.FindStringSubmatch(line); len(matches) == 5 {
			steamID, _ := strconv.ParseInt(matches[3], 10, 64)
			offlinePlayers = append(offlinePlayers, &OfflinePlayerInfo{
				SteamID:     steamID,
				EosID:       strings.TrimSpace(matches[2]),
				Nickname:    strings.TrimSpace(matches[4]),
				OfflineTime: time.Now(),
			})
		}
	}

	LogDebug("玩家解析: %d个在线, %d个离线", len(currentPlayers), len(offlinePlayers))
	return currentPlayers, offlinePlayers
}

func (pm *PlayerMonitor) updateDatabase(currentPlayers map[int]*PlayerInfo, offlinePlayers []*OfflinePlayerInfo) error {
	pm.onlinePlayersMutex.Lock()
	defer pm.onlinePlayersMutex.Unlock()

	var playersToAdd, playersToUpdate []*PlayerInfo
	var playersToRemove []int

	for gameID, player := range currentPlayers {
		if _, exists := pm.onlinePlayers[gameID]; !exists {
			playersToAdd = append(playersToAdd, player)
		} else if oldPlayer := pm.onlinePlayers[gameID]; !isPlayerInfoEqual(oldPlayer, player) {
			playersToUpdate = append(playersToUpdate, player)
		}
	}

	for gameID := range pm.onlinePlayers {
		if _, exists := currentPlayers[gameID]; !exists {
			playersToRemove = append(playersToRemove, gameID)
		}
	}

	pm.onlinePlayers = currentPlayers
	return pm.dbManager.UpdatePlayerDatabase(playersToAdd, playersToUpdate, playersToRemove, offlinePlayers)
}

func isPlayerInfoEqual(a, b *PlayerInfo) bool {
	if a.SteamID != b.SteamID || a.EosID != b.EosID || a.PlayerNickname != b.PlayerNickname || a.FactionID != b.FactionID {
		return false
	}
	if (a.SquadID == nil) != (b.SquadID == nil) {
		return false
	}
	if a.SquadID != nil && *a.SquadID != *b.SquadID {
		return false
	}
	return a.IsSquadLeader == b.IsSquadLeader && a.PlayerRole == b.PlayerRole
}

func (pm *PlayerMonitor) GetOnlinePlayersCount() int {
	pm.onlinePlayersMutex.RLock()
	defer pm.onlinePlayersMutex.RUnlock()
	return len(pm.onlinePlayers)
}

func (pm *PlayerMonitor) GetConnectionStatus() bool {
	return pm.isRunning && pm.conn != nil
}


type CombatPlayerInfo struct {
	Nickname string
	EosID    string
	SteamID  int64
	LastSeen time.Time
	IsActive bool
	IsOnline bool
}

type PendingLogEntry struct {
	LogType    string
	Line       string
	Matches    []string
	Timestamp  time.Time
	RetryCount int
}

type MemoryStats struct {
	PlayerHashMapSize   int
	MatchStatsMapSize   int
	PendingLogsSize     int
	GoRoutines          int
	AllocatedMemory     uint64
	SystemMemory        uint64
	LastCleanupTime     time.Time
	OnlinePlayersCount  int
	OfflinePlayersCount int
}

type CombatLogMonitor struct {
	cfg              *Config
	dbManager        *DBManager
	playerHashMap    map[string]*CombatPlayerInfo
	matchStatsMap    map[string]*PlayerMatchStats
	playerHashMutex  sync.RWMutex
	matchStatsMutex  sync.RWMutex
	currentMatchID   string
	logFile          *os.File
	fileOffset       int64
	isRunning        bool
	stopChan         chan struct{}
	lastSyncTime     time.Time
	pendingLogs      []PendingLogEntry
	pendingLogsMutex sync.Mutex
	lastMemoryReport time.Time
	lastCleanupTime  time.Time
	lastTPSRecord    time.Time
	tpsRecordMutex   sync.Mutex
	serverID         int
}

const (
	DEFAULT_PLAYER_MAP_CAPACITY = 1000
	MAX_PENDING_LOGS            = 1000
	PLAYER_EXPIRY_DURATION      = 1 * time.Hour
	CLEANUP_INTERVAL            = 10 * time.Minute
	MEMORY_REPORT_INTERVAL      = 15 * time.Minute
	MAX_RETRY_COUNT             = 5
	MAX_WAIT_TIME               = 30 * time.Second
	TPS_RECORD_INTERVAL         = 30 * time.Second
	TPS_CLEANUP_INTERVAL        = 24 * time.Hour
)

var (
	startNewGameRegex = regexp.MustCompile(`\[\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}:\d{3}\]\[\s*\d+\]LogSquad: StartNewGame`)
	reviveRegex       = regexp.MustCompile(`\[\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}:\d{3}\]\[\s*\d+\]LogSquad: (.*?) \(Online IDs: EOS: ([a-f0-9]*) steam: (\d+)\) has revived (.*?) \(Online IDs: EOS: ([a-f0-9]*) steam: (\d+)\)`)
	woundRegex        = regexp.MustCompile(`\[\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}:\d{3}\]\[\s*\d+\]LogSquadTrace: \[DedicatedServer\]Wound\(\): Player:(.*?) KillingDamage=([\d\.\-]+) from .* \(Online IDs: EOS: ([a-f0-9]*) steam: (\d+).*?\) caused by (.*)`)
	dieRegex          = regexp.MustCompile(`\[\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}:\d{3}\]\[\s*\d+\]LogSquadTrace: \[DedicatedServer\]Die\(\): Player:(.*?) KillingDamage=([\d\.\-]+) from .* \(Online IDs: EOS: ([a-f0-9]*) steam: (\d+).*?\) caused by (.*)`)
	timestampRegex    = regexp.MustCompile(`\[(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}):\d{3}\]`)
	tpsRegex          = regexp.MustCompile(`\[\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}:\d{3}\]\[\s*\d+\]LogSquad: USQGameState: Server Tick Rate: ([\d\.]+)`)
)

func NewCombatLogMonitor(cfg *Config, dbManager *DBManager) *CombatLogMonitor {
	return &CombatLogMonitor{
		cfg:              cfg,
		dbManager:        dbManager,
		playerHashMap:    make(map[string]*CombatPlayerInfo, DEFAULT_PLAYER_MAP_CAPACITY),
		matchStatsMap:    make(map[string]*PlayerMatchStats),
		stopChan:         make(chan struct{}),
		lastMemoryReport: time.Now(),
		lastCleanupTime:  time.Now(),
		lastTPSRecord:    time.Now(),
		serverID:         cfg.ServerID,
	}
}

func (clm *CombatLogMonitor) Start() error {
	LogInfo("正在启动战斗日志监控器...")

	if err := clm.initializeMatchID(); err != nil {
		return fmt.Errorf("初始化对局ID失败: %w", err)
	}

	if err := clm.syncPlayerData(); err != nil {
		LogWarn("初始同步玩家数据失败: %v", err)
	}

	logPath := filepath.Join(".", "SquadGame.log")
	file, err := os.Open(logPath)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	clm.logFile = file

	bom := make([]byte, 3)
	n, _ := file.Read(bom)
	if n == 3 && bom[0] == 0xEF && bom[1] == 0xBB && bom[2] == 0xBF {
		clm.fileOffset = 3
	} else {
		file.Seek(0, 0)
		clm.fileOffset = 0
	}

	offset, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("定位文件末尾失败: %w", err)
	}
	clm.fileOffset = offset

	clm.isRunning = true
	go clm.monitorLoop()

	LogInfo("战斗日志监控器已启动，当前对局ID: %s", clm.currentMatchID)
	return nil
}

func (clm *CombatLogMonitor) Stop() {
	if !clm.isRunning {
		return
	}
	LogInfo("正在停止战斗日志监控器...")
	clm.isRunning = false
	close(clm.stopChan)
	if clm.logFile != nil {
		clm.logFile.Close()
	}
	clm.logMemoryStats()
	LogInfo("战斗日志监控器已停止")
}

func (clm *CombatLogMonitor) initializeMatchID() error {
	var maxMatchID int64
	query := "SELECT COALESCE(MAX(CAST(match_id AS UNSIGNED)), 0) FROM sq_round_stats WHERE server_id = ?"
	err := clm.dbManager.GetDB().QueryRow(query, clm.serverID).Scan(&maxMatchID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	if maxMatchID == 0 {
		clm.currentMatchID = "1"
	} else {
		clm.currentMatchID = strconv.FormatInt(maxMatchID+1, 10)
	}

	LogInfo("初始化对局ID为: %s", clm.currentMatchID)
	return nil
}

func (clm *CombatLogMonitor) syncPlayerData() error {
	clm.playerHashMutex.Lock()
	defer clm.playerHashMutex.Unlock()

	newPlayerHashMap := make(map[string]*CombatPlayerInfo, DEFAULT_PLAYER_MAP_CAPACITY)
	currentTime := time.Now()

	query := `SELECT PlayerNickname, EosID, SteamID FROM sq_active_sessions WHERE server_id = ? AND EosID IS NOT NULL AND EosID != ''`
	rows, err := clm.dbManager.GetDB().Query(query, clm.serverID)
	if err != nil {
		return err
	}
	defer rows.Close()

	onlineCount := 0
	for rows.Next() {
		var player CombatPlayerInfo
		if err := rows.Scan(&player.Nickname, &player.EosID, &player.SteamID); err != nil {
			continue
		}
		player.LastSeen = currentTime
		player.IsActive = true
		player.IsOnline = true
		eosKey := strings.ToLower(player.EosID)
		newPlayerHashMap[eosKey] = &player
		onlineCount++
	}

	query2 := `SELECT player_name, eos_id, steam_id FROM sq_user_records WHERE server_id = ? AND eos_id IS NOT NULL AND eos_id != '' ORDER BY last_seen DESC LIMIT 30`
	rows2, err := clm.dbManager.GetDB().Query(query2, clm.serverID)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var player CombatPlayerInfo
			if err := rows2.Scan(&player.Nickname, &player.EosID, &player.SteamID); err != nil {
				continue
			}
			eosKey := strings.ToLower(player.EosID)
			if _, exists := newPlayerHashMap[eosKey]; !exists {
				player.LastSeen = currentTime.Add(-2 * time.Hour)
				player.IsActive = false
				player.IsOnline = false
				newPlayerHashMap[eosKey] = &player
			}
		}
	}

	clm.playerHashMap = newPlayerHashMap
	clm.lastSyncTime = time.Now()
	LogDebug("同步玩家数据完成，共加载 %d 个在线玩家", onlineCount)
	return nil
}

func (clm *CombatLogMonitor) monitorLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	syncTicker := time.NewTicker(3 * time.Second)
	cleanupTicker := time.NewTicker(CLEANUP_INTERVAL)
	memoryReportTicker := time.NewTicker(MEMORY_REPORT_INTERVAL)
	tpsCleanupTicker := time.NewTicker(TPS_CLEANUP_INTERVAL)

	defer ticker.Stop()
	defer syncTicker.Stop()
	defer cleanupTicker.Stop()
	defer memoryReportTicker.Stop()
	defer tpsCleanupTicker.Stop()

	reader := bufio.NewReader(clm.logFile)

	for {
		select {
		case <-clm.stopChan:
			return
		case <-syncTicker.C:
			if err := clm.syncPlayerData(); err != nil {
				LogWarn("同步玩家数据失败: %v", err)
			} else {
				clm.processPendingLogs()
			}
		case <-cleanupTicker.C:
			clm.cleanupExpiredPlayers()
		case <-memoryReportTicker.C:
			clm.logMemoryStats()
		case <-tpsCleanupTicker.C:
			if err := clm.dbManager.CleanOldTPSLogs(); err != nil {
				LogError("清理旧TPS数据失败: %v", err)
			}
		case <-ticker.C:
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					if err != io.EOF {
						LogError("读取日志文件失败: %v", err)
					}
					break
				}
				clm.processLogLine(strings.TrimSpace(line))
			}
		}
	}
}

func (clm *CombatLogMonitor) processLogLine(line string) {
	if line == "" {
		return
	}

	if startNewGameRegex.MatchString(line) {
		clm.handleNewGame()
		return
	}

	if matches := tpsRegex.FindStringSubmatch(line); len(matches) == 2 {
		clm.handleTPS(line, matches)
		return
	}

	if matches := reviveRegex.FindStringSubmatch(line); len(matches) == 7 {
		clm.handleRevive(line, matches)
		return
	}

	if matches := woundRegex.FindStringSubmatch(line); len(matches) == 6 {
		clm.handleWound(line, matches)
		return
	}

	if matches := dieRegex.FindStringSubmatch(line); len(matches) == 6 {
		clm.handleDie(line, matches)
		return
	}
}

func (clm *CombatLogMonitor) handleNewGame() {
	clm.matchStatsMutex.Lock()
	clm.matchStatsMap = make(map[string]*PlayerMatchStats)
	clm.matchStatsMutex.Unlock()

	oldMatchID := clm.currentMatchID
	newMatchIDInt, _ := strconv.ParseInt(clm.currentMatchID, 10, 64)
	clm.currentMatchID = strconv.FormatInt(newMatchIDInt+1, 10)

	LogInfo("检测到新对局开始! 对局ID: %s -> %s", oldMatchID, clm.currentMatchID)
}

func (clm *CombatLogMonitor) handleTPS(line string, matches []string) {
	clm.tpsRecordMutex.Lock()
	defer clm.tpsRecordMutex.Unlock()

	if time.Since(clm.lastTPSRecord) < TPS_RECORD_INTERVAL {
		return
	}

	tpsValue, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return
	}

	insertSQL := `INSERT INTO sq_perf_metrics (server_id, tps, recorded_at) VALUES (?, ?, NOW())`
	_, err = clm.dbManager.GetDB().Exec(insertSQL, clm.serverID, tpsValue)
	if err != nil {
		LogError("插入TPS记录失败: %v", err)
	} else {
		LogDebug("记录TPS: %.2f", tpsValue)
		clm.lastTPSRecord = time.Now()
	}
}

func (clm *CombatLogMonitor) handleRevive(line string, matches []string) {
	reviverName := strings.TrimSpace(matches[1])
	reviverEosID := strings.ToLower(strings.TrimSpace(matches[2]))
	reviverSteamIDStr := strings.TrimSpace(matches[3])
	revivedName := strings.TrimSpace(matches[4])
	revivedEosID := strings.ToLower(strings.TrimSpace(matches[5]))
	revivedSteamIDStr := strings.TrimSpace(matches[6])

	reviverSteamID, _ := strconv.ParseInt(reviverSteamIDStr, 10, 64)
	revivedSteamID, _ := strconv.ParseInt(revivedSteamIDStr, 10, 64)

	clm.playerHashMutex.Lock()
	currentTime := time.Now()
	if reviverEosID != "" {
		clm.playerHashMap[reviverEosID] = &CombatPlayerInfo{
			Nickname: reviverName,
			EosID:    reviverEosID,
			SteamID:  reviverSteamID,
			LastSeen: currentTime,
			IsActive: true,
			IsOnline: true,
		}
	}
	if revivedEosID != "" {
		clm.playerHashMap[revivedEosID] = &CombatPlayerInfo{
			Nickname: revivedName,
			EosID:    revivedEosID,
			SteamID:  revivedSteamID,
			LastSeen: currentTime,
			IsActive: true,
			IsOnline: true,
		}
	}
	clm.playerHashMutex.Unlock()

	clm.updatePlayerStats(reviverEosID, reviverName, reviverSteamID, 0, 0, 1, 0)

	eventTime := clm.extractTime(line)
	insertSQL := `INSERT INTO sq_heal_events (server_id, match_id, reviver_name, reviver_eos_id, reviver_steam_id, revived_name, revived_eos_id, revived_steam_id, revive_time) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := clm.dbManager.GetDB().Exec(insertSQL, clm.serverID, clm.currentMatchID, reviverName, reviverEosID, reviverSteamID, revivedName, revivedEosID, revivedSteamID, eventTime)
	if err != nil {
		LogError("插入救援日志失败: %v", err)
	} else {
		LogDebug("记录救援: %s -> %s", reviverName, revivedName)
	}
}

func (clm *CombatLogMonitor) handleWound(line string, matches []string) {
	victimFullName := strings.TrimSpace(matches[1])
	damage := strings.TrimSpace(matches[2])
	attackerEosID := strings.ToLower(strings.TrimSpace(matches[3]))
	attackerSteamIDStr := strings.TrimSpace(matches[4])
	weapon := strings.TrimSpace(matches[5])

	attackerSteamID, _ := strconv.ParseInt(attackerSteamIDStr, 10, 64)
	damageValue, _ := strconv.ParseFloat(damage, 64)

	clm.playerHashMutex.RLock()
	attackerInfo := clm.playerHashMap[attackerEosID]
	var attackerName string
	if attackerInfo != nil {
		attackerName = attackerInfo.Nickname
	}

	victimInfo, victimEosID, victimSteamID := clm.findVictimByName(victimFullName)
	var victimName string
	if victimInfo != nil {
		victimName = victimInfo.Nickname
	} else {
		victimName = victimFullName
	}
	clm.playerHashMutex.RUnlock()

	if attackerInfo == nil || victimInfo == nil {
		clm.addToPendingLogs("wound", line, matches)
		return
	}

	clm.updatePlayerStats(attackerEosID, attackerName, attackerSteamID, 0, 1, 0, 0)

	eventTime := clm.extractTime(line)
	insertSQL := `INSERT INTO sq_down_events (server_id, match_id, victim_name, victim_eos_id, victim_steam_id, attacker_name, attacker_eos_id, attacker_steam_id, weapon, damage, wound_time) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := clm.dbManager.GetDB().Exec(insertSQL, clm.serverID, clm.currentMatchID, victimName, victimEosID, victimSteamID, attackerName, attackerEosID, attackerSteamID, weapon, damageValue, eventTime)
	if err != nil {
		LogError("插入击倒日志失败: %v", err)
	} else {
		LogDebug("记录击倒: %s -> %s (%.1f)", attackerName, victimName, damageValue)
	}
}

func (clm *CombatLogMonitor) handleDie(line string, matches []string) {
	victimFullName := strings.TrimSpace(matches[1])
	damage := strings.TrimSpace(matches[2])
	attackerEosID := strings.ToLower(strings.TrimSpace(matches[3]))
	attackerSteamIDStr := strings.TrimSpace(matches[4])
	weapon := strings.TrimSpace(matches[5])

	attackerSteamID, _ := strconv.ParseInt(attackerSteamIDStr, 10, 64)
	damageValue, _ := strconv.ParseFloat(damage, 64)

	isSuicide := strings.Contains(weapon, "nullptr") || damageValue == 100.0

	clm.playerHashMutex.RLock()
	var attackerName, victimName, victimEosID string
	var victimSteamID int64

	if isSuicide {
		attackerInfo := clm.playerHashMap[attackerEosID]
		if attackerInfo != nil {
			attackerName = attackerInfo.Nickname
			victimName = attackerInfo.Nickname
			victimEosID = attackerInfo.EosID
			victimSteamID = attackerInfo.SteamID
		} else {
			victimInfo, foundEosID, foundSteamID := clm.findVictimByName(victimFullName)
			if victimInfo != nil {
				attackerName = victimInfo.Nickname
				victimName = victimInfo.Nickname
				victimEosID = foundEosID
				victimSteamID = foundSteamID
			} else {
				clm.playerHashMutex.RUnlock()
				clm.addToPendingLogs("die", line, matches)
				return
			}
		}
		weapon = "Suicide"
	} else {
		attackerInfo := clm.playerHashMap[attackerEosID]
		if attackerInfo == nil {
			clm.playerHashMutex.RUnlock()
			clm.addToPendingLogs("die", line, matches)
			return
		}
		attackerName = attackerInfo.Nickname

		victimInfo, foundEosID, foundSteamID := clm.findVictimByName(victimFullName)
		if victimInfo == nil {
			clm.playerHashMutex.RUnlock()
			clm.addToPendingLogs("die", line, matches)
			return
		}
		victimName = victimInfo.Nickname
		victimEosID = foundEosID
		victimSteamID = foundSteamID
	}
	clm.playerHashMutex.RUnlock()

	if !isSuicide {
		clm.updatePlayerStats(attackerEosID, attackerName, attackerSteamID, 1, 0, 0, 0)
	}
	if victimEosID != "" {
		clm.updatePlayerStats(victimEosID, victimName, victimSteamID, 0, 0, 0, 1)
	}

	eventTime := clm.extractTime(line)
	insertSQL := `INSERT INTO sq_frag_events (server_id, match_id, victim_name, victim_eos_id, victim_steam_id, killer_name, killer_eos_id, killer_steam_id, weapon, damage, kill_time) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := clm.dbManager.GetDB().Exec(insertSQL, clm.serverID, clm.currentMatchID, victimName, victimEosID, victimSteamID, attackerName, attackerEosID, attackerSteamID, weapon, damageValue, eventTime)
	if err != nil {
		LogError("插入击杀日志失败: %v", err)
	} else {
		if isSuicide {
			LogDebug("记录自杀: %s", victimName)
		} else {
			LogDebug("记录击杀: %s -> %s (%.1f)", attackerName, victimName, damageValue)
		}
	}
}

func (clm *CombatLogMonitor) findVictimByName(fullName string) (*CombatPlayerInfo, string, int64) {
	for _, player := range clm.playerHashMap {
		if player.Nickname == fullName {
			return player, player.EosID, player.SteamID
		}
	}

	parts := strings.Fields(fullName)
	for i := len(parts); i > 0; i-- {
		testName := strings.Join(parts[len(parts)-i:], " ")
		matchCount := 0
		var matchedPlayer *CombatPlayerInfo

		for _, player := range clm.playerHashMap {
			if strings.Contains(player.Nickname, testName) {
				matchCount++
				matchedPlayer = player
			}
		}

		if matchCount == 1 {
			return matchedPlayer, matchedPlayer.EosID, matchedPlayer.SteamID
		}
	}
	return nil, "", 0
}

func (clm *CombatLogMonitor) updatePlayerStats(eosID, name string, steamID int64, kills, wounds, revives, deaths int) {
	if eosID == "" {
		return
	}

	clm.matchStatsMutex.Lock()
	stats, exists := clm.matchStatsMap[eosID]
	if !exists {
		stats = &PlayerMatchStats{
			MatchID:       clm.currentMatchID,
			PlayerName:    name,
			PlayerSteamID: steamID,
			PlayerEosID:   eosID,
		}
		clm.matchStatsMap[eosID] = stats
	}
	stats.KillCount += kills
	stats.WoundCount += wounds
	stats.ReviveCount += revives
	stats.DeathCount += deaths
	clm.matchStatsMutex.Unlock()

	upsertSQL := `INSERT INTO sq_round_stats (server_id, match_id, player_name, player_steam_id, player_eos_id, kill_count, wound_count, death_count, revive_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE player_name=VALUES(player_name), kill_count=VALUES(kill_count), wound_count=VALUES(wound_count), death_count=VALUES(death_count), revive_count=VALUES(revive_count), updated_at=CURRENT_TIMESTAMP`
	clm.dbManager.GetDB().Exec(upsertSQL, clm.serverID, stats.MatchID, stats.PlayerName, stats.PlayerSteamID, stats.PlayerEosID, stats.KillCount, stats.WoundCount, stats.DeathCount, stats.ReviveCount)
}

func (clm *CombatLogMonitor) extractTime(line string) time.Time {
	matches := timestampRegex.FindStringSubmatch(line)
	if len(matches) > 1 {
		timeStr := matches[1]
		timeStr = strings.Replace(timeStr, ".", "-", 2)
		timeStr = strings.Replace(timeStr, "-", " ", 3)
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", timeStr, time.UTC); err == nil {
			return t
		}
	}
	return time.Now().UTC()
}

func (clm *CombatLogMonitor) addToPendingLogs(logType, line string, matches []string) {
	clm.pendingLogsMutex.Lock()
	defer clm.pendingLogsMutex.Unlock()

	if len(clm.pendingLogs) >= MAX_PENDING_LOGS {
		copy(clm.pendingLogs, clm.pendingLogs[1:])
		clm.pendingLogs = clm.pendingLogs[:len(clm.pendingLogs)-1]
	}

	clm.pendingLogs = append(clm.pendingLogs, PendingLogEntry{
		LogType:    logType,
		Line:       line,
		Matches:    append([]string(nil), matches...),
		Timestamp:  time.Now(),
		RetryCount: 0,
	})
}

func (clm *CombatLogMonitor) processPendingLogs() {
	clm.pendingLogsMutex.Lock()

	if len(clm.pendingLogs) == 0 {
		clm.pendingLogsMutex.Unlock()
		return
	}

	logsToProcess := make([]PendingLogEntry, len(clm.pendingLogs))
	copy(logsToProcess, clm.pendingLogs)
	clm.pendingLogs = nil
	clm.pendingLogsMutex.Unlock()

	processedCount := 0
	expiredCount := 0

	for _, pendingLog := range logsToProcess {
		if time.Since(pendingLog.Timestamp) > MAX_WAIT_TIME || pendingLog.RetryCount >= MAX_RETRY_COUNT {
			expiredCount++
			continue
		}

		switch pendingLog.LogType {
		case "revive":
			clm.handleRevive(pendingLog.Line, pendingLog.Matches)
		case "wound":
			clm.handleWound(pendingLog.Line, pendingLog.Matches)
		case "die":
			clm.handleDie(pendingLog.Line, pendingLog.Matches)
		}
		processedCount++
	}

	if processedCount > 0 || expiredCount > 0 {
		LogInfo("处理待处理日志: 尝试%d个, 过期%d个", processedCount, expiredCount)
	}
}

func (clm *CombatLogMonitor) cleanupExpiredPlayers() {
	clm.playerHashMutex.Lock()
	defer clm.playerHashMutex.Unlock()

	currentTime := time.Now()
	expiredCount := 0
	newPlayerHashMap := make(map[string]*CombatPlayerInfo)

	for eosID, player := range clm.playerHashMap {
		shouldKeep := player.IsOnline || player.IsActive || currentTime.Sub(player.LastSeen) < PLAYER_EXPIRY_DURATION
		if shouldKeep {
			newPlayerHashMap[eosID] = player
		} else {
			expiredCount++
		}
	}

	clm.playerHashMap = newPlayerHashMap
	clm.lastCleanupTime = currentTime

	if expiredCount > 0 {
		LogInfo("内存清理: 清理了%d个过期玩家, 剩余%d个", expiredCount, len(newPlayerHashMap))
	}
}

func (clm *CombatLogMonitor) logMemoryStats() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	clm.playerHashMutex.RLock()
	playerCount := len(clm.playerHashMap)
	onlineCount := 0
	for _, player := range clm.playerHashMap {
		if player.IsOnline {
			onlineCount++
		}
	}
	clm.playerHashMutex.RUnlock()

	clm.matchStatsMutex.RLock()
	matchStatsCount := len(clm.matchStatsMap)
	clm.matchStatsMutex.RUnlock()

	clm.pendingLogsMutex.Lock()
	pendingLogsCount := len(clm.pendingLogs)
	clm.pendingLogsMutex.Unlock()

	LogInfo("=== 战斗日志监控器内存统计 ===")
	LogInfo("  玩家缓存: %d (在线:%d)", playerCount, onlineCount)
	LogInfo("  对局统计: %d", matchStatsCount)
	LogInfo("  待处理队列: %d", pendingLogsCount)
	LogInfo("  分配内存: %.2f MB", float64(m.Alloc)/1024/1024)
}

func (clm *CombatLogMonitor) GetCurrentMatchID() string {
	return clm.currentMatchID
}

func (clm *CombatLogMonitor) GetPlayerCount() int {
	clm.playerHashMutex.RLock()
	defer clm.playerHashMutex.RUnlock()
	return len(clm.playerHashMap)
}

func (clm *CombatLogMonitor) GetTPSStats() (map[string]interface{}, error) {
	return clm.dbManager.GetLatestTPSStats()
}

func (clm *CombatLogMonitor) ForceCleanTPSLogs() error {
	LogInfo("执行强制TPS数据清理...")
	return clm.dbManager.CleanOldTPSLogs()
}
