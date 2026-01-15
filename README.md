# SquadRank 排位系统日志获取器

战术小队 (Squad) 游戏服务器排位系统的客户端日志采集器。

## 功能特性

- 实时监控在线玩家状态
- 记录击杀/击倒/救援事件
- 记录服务器TPS性能数据
- API心跳保活机制
- 自动清理过期数据

## 安全声明

本项目为开源版本，以下敏感信息已被移除或替换，使用前需要自行配置：

### 已移除/替换的配置项

| 类型 | 原始内容 | 替换为 | 说明 |
|------|---------|--------|------|
| API地址 | 完整API URL | `<YOUR_API_URL>` | 需配置你自己的API服务端点 |
| RCON主机 | 默认IP地址 | `<YOUR_RCON_HOST>` | 游戏服务器RCON地址 |
| RCON端口 | 默认端口号 | `0` (需填写) | 游戏服务器RCON端口 |
| RCON密码 | 示例密码 | `<YOUR_RCON_PASSWORD>` | 游戏服务器RCON密码 |
| 唯一标识 | 示例ID | `<YOUR_UNIQUE_ID>` | 服务器唯一标识符 |

### 数据库表名映射

为安全起见，数据库表名已进行混淆处理。如需使用，请根据以下映射创建对应的数据库表：

| 功能描述 | 开源版表名 |
|---------|-----------|
| 在线玩家会话 | `sq_active_sessions` |
| 玩家历史记录 | `sq_user_records` |
| 服务器性能指标 | `sq_perf_metrics` |
| 管理员操作日志 | `sq_admin_actions` |
| 击杀事件 | `sq_frag_events` |
| 击倒事件 | `sq_down_events` |
| 救援事件 | `sq_heal_events` |
| 对局统计 | `sq_round_stats` |

## 配置说明

### 1. 创建配置文件

首次运行程序会自动生成 `config.json` 模板：

```json
{
    "unique_id": "<YOUR_UNIQUE_ID>",
    "rcon": {
        "host": "<YOUR_RCON_HOST>",
        "port": 0,
        "password": "<YOUR_RCON_PASSWORD>"
    }
}
```

### 2. 配置API服务

你需要自行部署配套的API服务端，API应提供以下接口：

- `GET ?action=get_config&unique_id=xxx` - 获取服务器配置
- `POST ?action=register&unique_id=xxx&version=xxx` - 注册连接
- `POST ?action=heartbeat&unique_id=xxx&version=xxx` - 心跳保活
- `POST ?action=disconnect&unique_id=xxx` - 断开连接

### 3. 数据库配置

数据库连接信息通过API返回，API响应应包含：

```json
{
    "success": true,
    "data": {
        "server_id": 1,
        "server_name": "服务器名称",
        "database": {
            "host": "数据库主机",
            "port": 3306,
            "user": "用户名",
            "password": "密码",
            "name": "数据库名"
        }
    }
}
```

## 数据库表结构

### sq_active_sessions (在线玩家)

```sql
CREATE TABLE sq_active_sessions (
    id INT AUTO_INCREMENT PRIMARY KEY,
    server_id INT NOT NULL,
    GamePlayerID INT NOT NULL,
    SteamID BIGINT NOT NULL,
    EosID VARCHAR(64),
    PlayerNickname VARCHAR(128),
    FactionID INT,
    SquadID INT,
    IsSquadLeader VARCHAR(10),
    PlayerRole VARCHAR(64),
    UNIQUE KEY uk_server_player (server_id, GamePlayerID)
);
```

### sq_user_records (玩家历史)

```sql
CREATE TABLE sq_user_records (
    id INT AUTO_INCREMENT PRIMARY KEY,
    server_id INT NOT NULL,
    steam_id BIGINT NOT NULL,
    eos_id VARCHAR(64),
    player_name VARCHAR(128),
    is_online TINYINT DEFAULT 0,
    last_seen DATETIME,
    UNIQUE KEY uk_server_steam (server_id, steam_id)
);
```

### sq_round_stats (对局统计)

```sql
CREATE TABLE sq_round_stats (
    id INT AUTO_INCREMENT PRIMARY KEY,
    server_id INT NOT NULL,
    match_id VARCHAR(32) NOT NULL,
    player_name VARCHAR(128),
    player_steam_id BIGINT,
    player_eos_id VARCHAR(64),
    kill_count INT DEFAULT 0,
    wound_count INT DEFAULT 0,
    death_count INT DEFAULT 0,
    revive_count INT DEFAULT 0,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_match_player (server_id, match_id, player_eos_id)
);
```

### 其他表结构请参考代码中的SQL语句自行创建

## 编译运行

```bash
# 安装依赖
go mod download

# 编译
go build -o squad_rank_client .

# 运行
./squad_rank_client
```

## 依赖项

- Go 1.23+
- MySQL 5.7+ / MariaDB 10.3+
- github.com/go-sql-driver/mysql

## 许可证

MIT License

## 免责声明

本项目仅供学习交流使用，请确保你有权使用相关游戏服务器的RCON接口。作者不对任何滥用行为负责。
