# 日常运维

## 查看保护状态

在部署目录查看各仓库的状态和最近日志。

```sh
docker compose exec -T backup /bin/vaultwarden-less-backup status
docker compose logs --tail=100 backup
```

`source=ready` 表示 backup 取得了通过校验的数据库副本。每个目标都有自己的状态和时间记录。

| 输出 | 含义 |
| --- | --- |
| `protected` | 仓库已保存这份个人库内容。`checked_at` 是最近的比较时间，`snapshot_at` 是快照时间，长期没有改动时两者可以相隔数月。 |
| `checking` | 正在比较或上传。 |
| `failed` | 查看该目标的 `error` 和 `retry_at`。其余目标继续运行。 |
| `source=failed` | 本轮未能取到有效源库。各目标最后一次成功检查的时间说明已保护到何时。 |
| `source=unconfigured` | 没有可用的备份目标。[接入一个仓库](deployment.md#接入一个仓库) |

对象存储恢复后，backup 会在重试时追平当前内容。更改在线凭据后运行 `docker compose restart backup`。若调整了目标列表或通知设置，先用 `docker compose run --rm --no-deps backup validate` 检查，再重建 backup。[配置文件与生效方式](configuration.md)

`notification_errors` 按渠道列出投递问题。多个渠道同时报 `state_invalid`、`state_read_failed` 或 `state_write_failed` 时，日志会给出文件名。停下 backup，保留 `state/` 的副本，检查磁盘空间、文件属主和内容。`reports.json` 保存故障与提醒时间，`notifications.json` 保存待发消息与投递进度。修复后启动 backup 会续发。仅通知记录故障时备份照常进行。

## 校验仓库

以下命令在部署目录执行，每次选一个仓库。用实际文件名替换 `primary.env`。例子使用在线凭据，也可以用同一仓库的维护凭据。自定义的后端选项照 `backup.yaml` 传给 `restic -o key=value`。

```sh
docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
  --env-file ./credentials/backup/primary.env \
  --entrypoint /bin/restic vaultwarden-less-backup:development --no-cache snapshots
docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
  --env-file ./credentials/backup/primary.env \
  --entrypoint /bin/restic vaultwarden-less-backup:development --no-cache check --read-data
```

`check --read-data` 会下载并校验全部数据包，按仓库分开安排时间和流量。要确认某个时间点能找回条目，用客户端解锁一次恢复出的 vault。[从快照恢复](recovery.md)

## 保留与清理

先确认另一处仓库可以恢复。下面以 `credentials/maintenance/primary.env` 为例，使用有删除权限的维护凭据。先预览要保留的快照，示例按日留 7 份、按周留 5 份、按月留 12 份。按需要调整周期。

```sh
docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
  --env-file ./credentials/maintenance/primary.env \
  --entrypoint /bin/restic vaultwarden-less-backup:development \
  --no-cache forget --group-by '' --keep-daily 7 --keep-weekly 5 --keep-monthly 12 --dry-run
```

核对预览中的时间点后，用相同的保留条件执行 `forget`。需要回收未引用的数据时，再执行 `prune`。给这个仓库留出维护窗口，等当前上传结束。

```sh
docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
  --env-file ./credentials/maintenance/primary.env \
  --entrypoint /bin/restic vaultwarden-less-backup:development \
  --no-cache forget --group-by '' --keep-daily 7 --keep-weekly 5 --keep-monthly 12
docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
  --env-file ./credentials/maintenance/primary.env \
  --entrypoint /bin/restic vaultwarden-less-backup:development --no-cache prune
```

遇到锁冲突先核对正在运行的备份与维护进程。对象存储的生命周期策略应保留 restic 仍引用的对象。仓库损坏时，先从另一处恢复，保留故障仓库的副本，再按 restic 文档排查。[restic 仓库维护](https://restic.readthedocs.io/en/stable/045_working_with_repos.html) · [快照保留](https://restic.readthedocs.io/en/stable/060_forget.html)
