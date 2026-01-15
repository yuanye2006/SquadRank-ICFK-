package main

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type DBManager struct {
	db                   *sql.DB
	serverID             int
	timezone             *time.Location
	logQueue             chan interface{}
	statsLock            sync.RWMutex
	connStats            ConnectionStats
	lastConnectionReport time.Time
}

type ConnectionStats struct {
	TotalQueries    int64
	FailedQueries   int64
	LastQueryTime   time.Time
	LastError       string
	ReconnectCount  int
}

type PlayerInfo struct {
	GamePlayerID   int
	SteamID        int64
	EosID          string
	PlayerNickname string
	FactionID      int
	SquadID        *int
	IsSquadLeader  string
	PlayerRole     string
}

type OfflinePlayerInfo struct {
	SteamID     int64
	EosID       string
	Nickname    string
	OfflineTime time.Time
}

type PlayerMatchStats struct {
	MatchID       string
	PlayerName    string
	PlayerSteamID int64
	PlayerEosID   string
	KillCount     int
	WoundCount    int
	DeathCount    int
	ReviveCount   int
}

func NewDBManager(cfg *Config) (*DBManager, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=UTC&timeout=10s&readTimeout=30s&writeTimeout=30s",
		cfg.DBUser, cfg.DBPassword, cfg.DBHost, cfg.DBPort, cfg.DBName)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库连接失败")
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(3 * time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("数据库连接测试失败，请检查网络和配置")
	}

	tz, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		tz = time.FixedZone("CST", 8*3600)
	}

	mgr := &DBManager{
		db:                   db,
		serverID:             cfg.ServerID,
		timezone:             tz,
		logQueue:             make(chan interface{}, 1000),
		lastConnectionReport: time.Now(),
	}

	LogInfo("数据库连接成功")
	return mgr, nil
}

func (m *DBManager) GetDB() *sql.DB {
	return m.db
}

func (m *DBManager) GetServerID() int {
	return m.serverID
}

func (m *DBManager) GetDBTime(t time.Time) time.Time {
	return t.In(m.timezone)
}

func (m *DBManager) Close() {
	if m.db != nil {
		m.db.Close()
	}
	close(m.logQueue)
	LogInfo("数据库连接已关闭")
}

func (m *DBManager) LogConnectionStats() {
	m.statsLock.RLock()
	defer m.statsLock.RUnlock()

	stats := m.db.Stats()
	LogInfo("数据库连接池: 打开=%d, 使用中=%d, 空闲=%d",
		stats.OpenConnections, stats.InUse, stats.Idle)
}


func (m *DBManager) ClearAllOnlinePlayers() error {
	_, err := m.db.Exec("DELETE FROM sq_active_sessions WHERE server_id = ?", m.serverID)
	if err != nil {
		return fmt.Errorf("清空在线玩家表失败: %w", err)
	}
	LogInfo("已清空在线玩家表")
	return nil
}

func (m *DBManager) UpdatePlayerDatabase(playersToAdd, playersToUpdate []*PlayerInfo, playersToRemove []int, offlinePlayers []*OfflinePlayerInfo) error {
	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("开始事务失败: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	if len(playersToAdd) > 0 {
		insertStmt, err := tx.Prepare(`INSERT INTO sq_active_sessions (server_id, GamePlayerID, SteamID, EosID, PlayerNickname, FactionID, SquadID, IsSquadLeader, PlayerRole) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE SteamID=VALUES(SteamID), EosID=VALUES(EosID), PlayerNickname=VALUES(PlayerNickname), FactionID=VALUES(FactionID), SquadID=VALUES(SquadID), IsSquadLeader=VALUES(IsSquadLeader), PlayerRole=VALUES(PlayerRole)`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("准备插入语句失败: %w", err)
		}
		defer insertStmt.Close()

		for _, player := range playersToAdd {
			_, err := insertStmt.Exec(m.serverID, player.GamePlayerID, player.SteamID, player.EosID, player.PlayerNickname, player.FactionID, player.SquadID, player.IsSquadLeader, player.PlayerRole)
			if err != nil {
				LogError("插入玩家失败 [%s]: %v", player.PlayerNickname, err)
			} else {
				LogInfo("玩家加入: %s (Steam: %d)", player.PlayerNickname, player.SteamID)
			}
		}
	}

	if len(playersToUpdate) > 0 {
		updateStmt, err := tx.Prepare(`UPDATE sq_active_sessions SET SteamID=?, EosID=?, PlayerNickname=?, FactionID=?, SquadID=?, IsSquadLeader=?, PlayerRole=? WHERE server_id=? AND GamePlayerID=?`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("准备更新语句失败: %w", err)
		}
		defer updateStmt.Close()

		for _, player := range playersToUpdate {
			_, err := updateStmt.Exec(player.SteamID, player.EosID, player.PlayerNickname, player.FactionID, player.SquadID, player.IsSquadLeader, player.PlayerRole, m.serverID, player.GamePlayerID)
			if err != nil {
				LogError("更新玩家失败 [%s]: %v", player.PlayerNickname, err)
			}
		}
	}

	if len(playersToRemove) > 0 {
		deleteStmt, err := tx.Prepare(`DELETE FROM sq_active_sessions WHERE server_id=? AND GamePlayerID=?`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("准备删除语句失败: %w", err)
		}
		defer deleteStmt.Close()

		for _, gameID := range playersToRemove {
			_, err := deleteStmt.Exec(m.serverID, gameID)
			if err != nil {
				LogError("删除玩家失败 [GameID=%d]: %v", gameID, err)
			} else {
				LogDebug("玩家离开: GameID=%d", gameID)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}

	go m.syncToPlayersHistory(playersToAdd, playersToUpdate)

	if len(offlinePlayers) > 0 {
		go m.updateOfflinePlayers(offlinePlayers)
	}

	return nil
}

func (m *DBManager) syncToPlayersHistory(playersToAdd, playersToUpdate []*PlayerInfo) {
	allPlayers := append(playersToAdd, playersToUpdate...)
	if len(allPlayers) == 0 {
		return
	}

	for _, player := range allPlayers {
		var existingID int
		err := m.db.QueryRow("SELECT id FROM sq_user_records WHERE server_id = ? AND steam_id = ?", m.serverID, player.SteamID).Scan(&existingID)

		if err == sql.ErrNoRows {
			_, err = m.db.Exec(`INSERT INTO sq_user_records (server_id, steam_id, eos_id, player_name, is_online, last_seen) VALUES (?, ?, ?, ?, 1, NOW())`,
				m.serverID, player.SteamID, player.EosID, player.PlayerNickname)
			if err != nil {
				LogError("插入历史玩家失败: %v", err)
			}
		} else if err == nil {
			_, err = m.db.Exec(`UPDATE sq_user_records SET eos_id=?, player_name=?, is_online=1, last_seen=NOW() WHERE id=?`,
				player.EosID, player.PlayerNickname, existingID)
			if err != nil {
				LogError("更新历史玩家失败: %v", err)
			}
		}
	}
}

func (m *DBManager) updateOfflinePlayers(offlinePlayers []*OfflinePlayerInfo) {
	for _, player := range offlinePlayers {
		_, err := m.db.Exec(`UPDATE sq_user_records SET is_online=0, last_seen=NOW() WHERE server_id=? AND steam_id=?`,
			m.serverID, player.SteamID)
		if err != nil {
			LogError("更新离线玩家失败: %v", err)
		}
	}
}

func (m *DBManager) GetOnlinePlayersCount() (int, error) {
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM sq_active_sessions WHERE server_id = ?", m.serverID).Scan(&count)
	return count, err
}


func (m *DBManager) GetLatestTPSStats() (map[string]interface{}, error) {
	result := make(map[string]interface{})

	var latestTPS sql.NullFloat64
	err := m.db.QueryRow("SELECT tps FROM sq_perf_metrics WHERE server_id = ? ORDER BY recorded_at DESC LIMIT 1", m.serverID).Scan(&latestTPS)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if latestTPS.Valid {
		result["latest_tps"] = latestTPS.Float64
	} else {
		result["latest_tps"] = -1.0
	}

	var avgTPS sql.NullFloat64
	err = m.db.QueryRow("SELECT AVG(tps) FROM sq_perf_metrics WHERE server_id = ? AND recorded_at > DATE_SUB(NOW(), INTERVAL 1 HOUR)", m.serverID).Scan(&avgTPS)
	if err == nil && avgTPS.Valid {
		result["avg_tps_1h"] = avgTPS.Float64
	}

	return result, nil
}

func (m *DBManager) CleanOldTPSLogs() error {
	result, err := m.db.Exec("DELETE FROM sq_perf_metrics WHERE server_id = ? AND recorded_at < DATE_SUB(NOW(), INTERVAL 7 DAY)", m.serverID)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected > 0 {
		LogInfo("清理了 %d 条旧TPS记录", affected)
	}
	return nil
}


func (m *DBManager) CheckAndCloseOfflineAdminFlights() error {
	rows, err := m.db.Query(`SELECT LogID, SteamID FROM sq_admin_actions WHERE server_id = ? AND FlightEndTime IS NULL`, m.serverID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var logsToClose []int

	for rows.Next() {
		var logID int
		var steamID string
		if err := rows.Scan(&logID, &steamID); err != nil {
			continue
		}

		var count int
		err := m.db.QueryRow("SELECT COUNT(*) FROM sq_active_sessions WHERE server_id = ? AND SteamID = ?", m.serverID, steamID).Scan(&count)
		if err != nil || count == 0 {
			logsToClose = append(logsToClose, logID)
		}
	}

	for _, logID := range logsToClose {
		_, err := m.db.Exec("UPDATE sq_admin_actions SET FlightEndTime = NOW() WHERE LogID = ?", logID)
		if err != nil {
			LogError("关闭飞天记录失败 [LogID=%d]: %v", logID, err)
		} else {
			LogInfo("关闭离线管理员飞天记录: LogID=%d", logID)
		}
	}

	return nil
}
