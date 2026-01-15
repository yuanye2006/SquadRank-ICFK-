package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const VERSION = "2.4.1-lite"
const API_URL = "<YOUR_API_URL>"
const CONFIG_FILE = "config.json"

type LocalRCONConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Password string `json:"password"`
}

type LocalConfig struct {
	UniqueID string          `json:"unique_id"`
	RCON     LocalRCONConfig `json:"rcon"`
}

type Config struct {
	ServerID          int
	ServerName        string
	UniqueID          string
	RCONHost          string
	RCONHost1         string
	RCONHost2         string
	RCONHost3         string
	RCONPort          int
	RCONPassword      string
	RCONRequestIDSeed int32
	LogLevel          string
	DBHost            string
	DBPort            int
	DBUser            string
	DBPassword        string
	DBName            string
	DBTimezone        string
	CombatLogPath     string
	ExpiresAt         string
}

type APIResponse struct {
	Success   bool            `json:"success"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	Timestamp int64           `json:"timestamp"`
}

type APIConfigData struct {
	ServerID   int    `json:"server_id"`
	ServerName string `json:"server_name"`
	UniqueID   string `json:"unique_id"`
	RCON       struct {
		Host     string `json:"host"`
		Host1    string `json:"host1"`
		Host2    string `json:"host2"`
		Host3    string `json:"host3"`
		Port     int    `json:"port"`
		Password string `json:"password"`
	} `json:"rcon"`
	Database struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		Name     string `json:"name"`
	} `json:"database"`
	CombatLogPath string `json:"combat_log_path"`
	RequestIDSeed int    `json:"request_id_seed"`
	LogLevel      string `json:"log_level"`
	ExpiresAt     string `json:"expires_at"`
}

func LoadLocalConfig() (*LocalConfig, error) {
	if _, err := os.Stat(CONFIG_FILE); os.IsNotExist(err) {
		defaultConfig := &LocalConfig{
			UniqueID: "<YOUR_UNIQUE_ID>",
			RCON: LocalRCONConfig{
				Host:     "<YOUR_RCON_HOST>",
				Port:     0,
				Password: "<YOUR_RCON_PASSWORD>",
			},
		}

		data, _ := json.MarshalIndent(defaultConfig, "", "    ")
		if err := os.WriteFile(CONFIG_FILE, data, 0644); err != nil {
			return nil, fmt.Errorf("创建配置文件失败: %v", err)
		}

		fmt.Println("=========================================")
		fmt.Println("已创建配置文件: config.json")
		fmt.Println("请编辑 config.json 填写以下配置:")
		fmt.Println("  - unique_id: 服务器唯一标识")
		fmt.Println("  - rcon.host: RCON地址")
		fmt.Println("  - rcon.port: RCON端口")
		fmt.Println("  - rcon.password: RCON密码")
		fmt.Println("然后重新运行程序")
		fmt.Println("=========================================")
		os.Exit(0)
	}

	data, err := os.ReadFile(CONFIG_FILE)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %v", err)
	}

	var localConfig LocalConfig
	if err := json.Unmarshal(data, &localConfig); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %v", err)
	}

	if localConfig.UniqueID == "" || localConfig.UniqueID == "<YOUR_UNIQUE_ID>" {
		return nil, fmt.Errorf("请在 config.json 中填写有效的 unique_id")
	}

	if localConfig.RCON.Host == "" || localConfig.RCON.Host == "<YOUR_RCON_HOST>" {
		return nil, fmt.Errorf("请在 config.json 中填写有效的 rcon.host")
	}
	if localConfig.RCON.Password == "" || localConfig.RCON.Password == "<YOUR_RCON_PASSWORD>" {
		return nil, fmt.Errorf("请在 config.json 中填写有效的 rcon.password")
	}
	if localConfig.RCON.Port == 0 {
		return nil, fmt.Errorf("请在 config.json 中填写有效的 rcon.port")
	}

	return &localConfig, nil
}

func FetchConfigFromAPI(uniqueID string) (*Config, error) {
	reqURL := fmt.Sprintf("%s?action=get_config&unique_id=%s", API_URL, url.QueryEscape(uniqueID))

	LogInfo("正在从API获取配置...")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("请求API失败，请检查网络连接")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败")
	}

	var apiResp APIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("解析响应失败")
	}

	if !apiResp.Success {
		return nil, fmt.Errorf("API返回错误: %s", apiResp.Message)
	}

	var configData APIConfigData
	if err := json.Unmarshal(apiResp.Data, &configData); err != nil {
		return nil, fmt.Errorf("解析配置数据失败: %v", err)
	}

	config := &Config{
		ServerID:          configData.ServerID,
		ServerName:        configData.ServerName,
		UniqueID:          configData.UniqueID,
		RCONHost:          configData.RCON.Host,
		RCONHost1:         configData.RCON.Host1,
		RCONHost2:         configData.RCON.Host2,
		RCONHost3:         configData.RCON.Host3,
		RCONPort:          configData.RCON.Port,
		RCONPassword:      configData.RCON.Password,
		RCONRequestIDSeed: int32(configData.RequestIDSeed),
		LogLevel:          configData.LogLevel,
		DBHost:            configData.Database.Host,
		DBPort:            configData.Database.Port,
		DBUser:            configData.Database.User,
		DBPassword:        configData.Database.Password,
		DBName:            configData.Database.Name,
		DBTimezone:        "Asia/Shanghai",
		CombatLogPath:     configData.CombatLogPath,
		ExpiresAt:         configData.ExpiresAt,
	}

	if config.RCONHost1 == "" {
		config.RCONHost1 = config.RCONHost
	}
	if config.RCONHost2 == "" {
		config.RCONHost2 = config.RCONHost
	}
	if config.RCONHost3 == "" {
		config.RCONHost3 = config.RCONHost
	}
	if config.RCONPort == 0 {
		return nil, fmt.Errorf("RCON端口未配置")
	}
	if config.DBPort == 0 {
		config.DBPort = 3306
	}
	if config.RCONRequestIDSeed == 0 {
		config.RCONRequestIDSeed = 1000
	}
	if config.LogLevel == "" {
		config.LogLevel = "INFO"
	}

	LogInfo("配置获取成功")
	return config, nil
}

func RegisterConnection(uniqueID string) error {
	reqURL := fmt.Sprintf("%s?action=register&unique_id=%s&version=%s",
		API_URL, url.QueryEscape(uniqueID), url.QueryEscape(VERSION))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(reqURL, "application/x-www-form-urlencoded", nil)
	if err != nil {
		return fmt.Errorf("注册连接失败，请检查网络")
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var apiResp APIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return fmt.Errorf("解析响应失败")
	}

	if !apiResp.Success {
		return fmt.Errorf("注册失败: %s", apiResp.Message)
	}

	LogInfo("已注册连接到服务器")
	return nil
}

func SendHeartbeat(uniqueID string) error {
	reqURL := fmt.Sprintf("%s?action=heartbeat&unique_id=%s&version=%s",
		API_URL, url.QueryEscape(uniqueID), url.QueryEscape(VERSION))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(reqURL, "application/x-www-form-urlencoded", nil)
	if err != nil {
		return fmt.Errorf("心跳发送失败")
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var apiResp APIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return fmt.Errorf("解析响应失败")
	}

	if !apiResp.Success {
		return fmt.Errorf("心跳失败: %s", apiResp.Message)
	}

	return nil
}

func DisconnectFromAPI(uniqueID string) error {
	reqURL := fmt.Sprintf("%s?action=disconnect&unique_id=%s",
		API_URL, url.QueryEscape(uniqueID))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(reqURL, "application/x-www-form-urlencoded", nil)
	if err != nil {
		return fmt.Errorf("断开连接失败")
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var apiResp APIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return fmt.Errorf("解析响应失败")
	}

	if !apiResp.Success {
		return fmt.Errorf("断开失败: %s", apiResp.Message)
	}

	LogInfo("已断开与服务器的连接")
	return nil
}

type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

var currentLogLevel LogLevel = LogLevelInfo

func SetLogLevel(levelStr string) {
	parsedLevel := LogLevelInfo
	valid := true

	switch strings.ToUpper(levelStr) {
	case "DEBUG":
		parsedLevel = LogLevelDebug
	case "INFO":
		parsedLevel = LogLevelInfo
	case "WARN":
		parsedLevel = LogLevelWarn
	case "ERROR":
		parsedLevel = LogLevelError
	default:
		valid = false
		fmt.Printf("%s [系统警告] 无效的日志级别 '%s'，已自动设为 INFO。\n", time.Now().Format("2006-01-02 15:04:05"), levelStr)
	}

	if currentLogLevel != parsedLevel && valid {
		currentLogLevel = parsedLevel
		LogInfo("日志级别已设置为: %s", strings.ToUpper(levelStr))
	} else {
		currentLogLevel = parsedLevel
	}
}

func logPrefix(level string) string {
	return fmt.Sprintf("%s [%s] ", time.Now().Format("2006-01-02 15:04:05"), level)
}

func LogDebug(format string, args ...interface{}) {
	if currentLogLevel <= LogLevelDebug {
		fmt.Printf(logPrefix("DEBUG")+format+"\n", args...)
	}
}

func LogInfo(format string, args ...interface{}) {
	if currentLogLevel <= LogLevelInfo {
		fmt.Printf(logPrefix("INFO")+format+"\n", args...)
	}
}

func LogWarn(format string, args ...interface{}) {
	if currentLogLevel <= LogLevelWarn {
		fmt.Printf(logPrefix("WARN")+format+"\n", args...)
	}
}

func LogError(format string, args ...interface{}) {
	if currentLogLevel <= LogLevelError {
		fmt.Printf(logPrefix("ERROR")+format+"\n", args...)
	}
}
