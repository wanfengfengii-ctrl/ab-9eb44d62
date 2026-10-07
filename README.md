# Schema Registry — 遥测二进制字段模式注册服务

集中发布遥测采集平台的二进制字段模式（schema）。保证设备升级后：

- 已删除字段的编号**永远 tombstone**，不会被新含义占用；
- 同一 subject 不会出现两条"当前版本"（并发后继版本只接纳一个）；
- 相同 `requestId` 的重复提交安全回放，不会重复生效；
- 已接纳状态落盘，服务重启后仍可查询。

纯 Go 标准库实现，无外部依赖。

## API

### `POST /api/schemas/{subject}/versions`

提交新版本。请求体：

```json
{
  "requestId": "deploy-2026-10-07-001",
  "version": 2,
  "fields": [
    {"name": "temp_c", "number": 1, "wireType": "VARINT"},
    {"name": "humidity", "number": 4, "wireType": "FIXED32"}
  ],
  "reservedNumbers": [2, 3]
}
```

- `requestId`：幂等键（同一 subject 下）。
- `version`：首版必须为 `1`，之后必须等于当前版本 + 1。
- `fields[].wireType`：四类线型之一 —— `VARINT`、`FIXED32`、`FIXED64`、`LENGTH_DELIMITED`（大小写不敏感，`-`/`_` 等价）。
- `reservedNumbers`：本版本起保留（tombstone）的编号。删除字段时必须把其编号列入；历史保留编号必须全部继续列出。

成功返回 `201`：

```json
{
  "subject": "device.telemetry",
  "version": 2,
  "fields": [...],
  "reservedNumbers": [2, 3],
  "createdAt": "2026-10-07T14:00:00Z"
}
```

### `GET /api/schemas/{subject}`

返回当前版本与累计保留编号：

```json
{
  "subject": "device.telemetry",
  "currentVersion": 2,
  "fields": [...],
  "reservedNumbers": [2, 3]
}
```

未知 subject 返回 `404` + `SUBJECT_NOT_FOUND`。

### `GET /healthz`

健康检查，返回 `200 {"status":"ok"}`。

## 演进规则

按固定顺序校验，首个失败的规则决定错误码，`firstViolation` 为该规则下**最小的违规编号**：

| 顺序 | 规则 | 错误码 |
|---|---|---|
| 1 | 删除的字段编号必须列入 `reservedNumbers` | `NUMBER_NOT_RETAINED` |
| 2 | 历史保留编号不得撤销 | `RESERVED_NUMBER_REVOKED` |
| 3 | 保留编号不得作为字段复用 | `RESERVED_NUMBER_REUSED` |
| 4 | 既有编号不得改名 | `FIELD_RENAMED` |
| 5 | 既有编号不得改变线型 | `WIRE_TYPE_CHANGED` |

版本与请求级规则：

| 规则 | 错误码 | HTTP |
|---|---|---|
| 首版必须为 1 / 后续版本必须为当前+1 | `VERSION_NOT_NEXT` | 409 |
| 相同 requestId、不同内容 | `REQUEST_CONFLICT` | 409 |
| 字段编号/名称/保留号重复、非法线型等 | `DUPLICATE_FIELD_NUMBER` / `DUPLICATE_FIELD_NAME` / `DUPLICATE_RESERVED_NUMBER` / `VALIDATION_ERROR` | 400 |

错误体统一为：

```json
{
  "error": {
    "code": "NUMBER_NOT_RETAINED",
    "message": "field number 3 was removed without retaining its number in reservedNumbers",
    "currentVersion": 1,
    "firstViolation": 3
  }
}
```

**失败请求不会改变该 subject 的当前模式或编号墓碑**（校验全部通过后才一次性提交；持久化失败会回滚内存状态）。

## 幂等与并发

- 相同 subject + 相同 `requestId` + 相同内容：回放原始结果（原状态码与响应体，响应头带 `X-Idempotent-Replay: true`）。内容指纹对字段顺序、保留号顺序不敏感。
- 相同 `requestId` + 不同内容：`409 REQUEST_CONFLICT`。
- 并发提交同一后继版本：单互斥锁串行化校验+提交，**恰好一个** `201`，其余 `409 VERSION_NOT_NEXT`。
- 被接纳与被拒绝（格式合法）的结果都记录在 `requestId` 下并持久化，重启后回放仍然有效。

## 持久化

状态写入 `${DATA_DIR}/schemas.json`（默认 `./data/schemas.json`，可用 `DATA_FILE` 覆盖）：临时文件 + fsync + rename 原子替换，随后目录 fsync。重启后已接纳状态可查询。

## 配置

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `PORT` | `8080` | 容器内监听端口 |
| `DATA_DIR` | `./data`（镜像内 `/data`） | 状态文件目录 |
| `DATA_FILE` | `$DATA_DIR/schemas.json` | 状态文件完整路径 |
| `API_PORT`（compose） | `8080` | **宿主机**映射端口 |

## 运行

```bash
# Docker Compose（宿主机端口可配置）
API_PORT=9000 docker compose up -d api

curl -s http://localhost:9000/healthz
curl -s -X POST http://localhost:9000/api/schemas/device.telemetry/versions \
  -d '{"requestId":"r1","version":1,"fields":[{"name":"temp_c","number":1,"wireType":"VARINT"}]}'
curl -s http://localhost:9000/api/schemas/device.telemetry

# 本地（无 Docker）
go build -o server . && PORT=8080 ./server
```

## 一次性验证（verify 服务）

`verify` 服务等待 API 健康后依次执行：构建检查（`go build` + `go vet`）、单元测试（`go test`）、以及针对运行中 API 的真实发布 / 演进 / 幂等回放 / 冲突 / 并发冒烟，并以退出码报告结果（0 = 全部通过）：

```bash
docker compose up --exit-code-from verify verify
echo $?   # 0 = VERIFY OK
```

冒烟每次运行使用唯一 subject，因此**可重复运行**：

```bash
docker compose up --exit-code-from verify verify   # 再次运行仍然通过
```

## 项目结构

```
main.go         进程入口、配置、优雅退出
server.go       HTTP 路由与 handler
schema.go       类型、四类线型、请求校验与演进规则
store.go        互斥锁保护的状态机 + 原子文件持久化 + 幂等记录
*_test.go       单元 / 集成 / 并发 / 重启持久化测试
smoke/main.go   端到端冒烟（发布、演进、冲突、并发竞争）
verify.sh       verify 服务入口：构建检查 → 测试 → 冒烟
Dockerfile      多阶段：build / runtime(alpine, 非 root) / verify
docker-compose.yml  api（健康检查、可配端口、数据卷）+ verify
```
