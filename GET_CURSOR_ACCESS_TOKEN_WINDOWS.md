# Windows 获取 Cursor Access Token

本文说明如何从**当前 Windows 用户已登录的 Cursor 客户端**中读取 Access Token，并将它配置给 cursor2api。

> Access Token 等同于账号凭据。只应读取你自己的账号，不要截图、发送给他人，也不要提交到 Git。操作期间不要修改 Cursor 数据库。

## 1. 安装 SQLite 数据库查看器

推荐使用开源工具 **DB Browser for SQLite**：

- 官方下载页：https://sqlitebrowser.org/dl/
- GitHub 最新版本：https://github.com/sqlitebrowser/sqlitebrowser/releases/latest

在下载页选择 Windows 64 位安装版或便携版均可。请只从官方网站或其 GitHub Releases 下载。

## 2. 完全退出 Cursor

先保存工作，然后退出 Cursor。若 Cursor 仍留在系统托盘，也要从托盘退出。

可以在任务管理器中确认没有正在运行的 `Cursor.exe`。这样可以避免数据库被占用，也能确保最新登录状态已经写入数据库。

## 3. 打开 Cursor 状态数据库

1. 启动 **DB Browser for SQLite**。
2. 点击 **Open Database（打开数据库）**。
3. 在文件选择框中输入以下路径：

   ```text
   %APPDATA%\Cursor\User\globalStorage\state.vscdb
   ```

   对应的完整路径通常是：

   ```text
   C:\Users\你的用户名\AppData\Roaming\Cursor\User\globalStorage\state.vscdb
   ```

4. 如果文件选择框看不到 `state.vscdb`，将文件类型切换为 **All files（所有文件）**。

建议只查看数据，不要点击 **Write Changes（写入更改）**。

## 4. 查找 Access Token

### 方法一：在表格中筛选

1. 打开 **Browse Data（浏览数据）** 标签。
2. 在 **Table（表）** 下拉框中选择 `ItemTable`。
3. 在 `key` 列的筛选框中输入 `accessToken`。
4. 找到键名为 `cursorAuth/accessToken` 的记录。
5. 复制该记录 `value` 列中的完整内容，这就是要配置的 Access Token。

不要复制 `refreshToken`、邮箱或其他记录，也不要对 `value` 做 Base64 解码。

### 方法二：执行 SQL

打开 **Execute SQL（执行 SQL）** 标签，执行：

```sql
SELECT key, CAST(value AS TEXT) AS value
FROM ItemTable
WHERE key LIKE '%accessToken%';
```

结果中通常会出现 `cursorAuth/accessToken`。复制其 `value` 单元格的完整内容。

## 5. 配置 cursor2api

仅对当前 PowerShell 窗口生效：

```powershell
$env:CURSOR_ACCESS_TOKEN = "在这里粘贴刚才复制的值"
.\cursor2api.exe .\config.json
```

服务启动后如果成功输出类似下面的日志，说明 token 可以正常访问 Cursor 后端：

```text
cursor: 211 usable models
```

模型数量会随账号和 Cursor 后端变化，不要求一定是 `211`。

在 VPS 或 Docker 中使用时，应把 token 放入仅服务器管理员可读的环境文件，例如 `.env.cursor2api`：

```dotenv
CURSOR_ACCESS_TOKEN=在这里粘贴刚才复制的值
```

修改环境文件后需要重新创建容器，单纯重启旧容器不会重新读取 Compose 环境变量：

```bash
docker compose up -d --no-build --force-recreate
```

## 6. 常见问题

### 查询不到 `accessToken`

- 确认 Cursor 客户端已经登录。
- 登录后正常退出一次 Cursor，再重新打开数据库。
- 确认打开的是当前 Windows 用户的 `%APPDATA%` 路径。
- Cursor 后续版本可能调整存储键名，可先用 `accessToken` 模糊筛选，不要直接修改数据库。

### token 返回 401

该 token 可能已经过期或被轮换。重新登录 Cursor，退出客户端后再次从数据库读取最新值，然后更新 cursor2api 的环境变量并重启服务。

### 安全清理

- 关闭 DB Browser，不要保存对数据库的修改。
- 清除剪贴板中的 token。
- 不要把 token 放进 `config.json`、命令历史、日志、Issue 或聊天消息。
- `.env.cursor2api` 应限制文件权限，并且不得提交到 Git。
