# Mini-Drop 备份恢复与数据保留

## PostgreSQL

备份：

```bash
pg_dump "$PG_DSN" --format=custom --file=mini-drop-$(date +%Y%m%d).dump
```

恢复：

```bash
createdb drop_restore
pg_restore --dbname="$RESTORE_PG_DSN" --clean --if-exists mini-drop-YYYYMMDD.dump
```

恢复后先启动 apiserver，等待 migration 检查通过，再启动 analysis 和 Web。

## MinIO / S3 Artifact

本地 MinIO 可用 `mc mirror`：

```bash
mc alias set src http://localhost:9000 "$S3_ACCESS_KEY" "$S3_SECRET_KEY"
mc mirror src/drop-data ./backup/drop-data
```

恢复：

```bash
mc alias set dst http://localhost:9000 "$S3_ACCESS_KEY" "$S3_SECRET_KEY"
mc mb --ignore-existing dst/drop-data
mc mirror ./backup/drop-data dst/drop-data
```

生产对象存储建议开启版本控制、服务端加密和跨区复制。

## 数据保留策略

- RAW artifact 默认保留 30 天，结果 artifact 默认保留 90 天。
- 审计和状态事件至少保留 180 天。
- 清理顺序必须先确认 DB 中 artifact retention/status，再删除对象存储，最后写清理审计。
- 演示环境可使用 `docker compose down -v` 清空全部数据；生产环境不要使用该命令。

## 恢复验收

1. `/readyz` 返回 ready。
2. 任务详情能看到状态时间线和 attempts。
3. 至少一个历史 Artifact 能生成下载 URL。
4. 新建一条短任务，确认采集、上传、分析都能完成。
