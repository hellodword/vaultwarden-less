## restore from backup

I'm using restic, so:

```sh
# disable bash history
export HISTFILE=/dev/null

# using aws for example:
# find the snapshot ID
env RESTIC_PASSWORD="..." RESTIC_REPOSITORY="s3:s3.amazonaws.com/bucketname/somepath" AWS_ACCESS_KEY_ID="..." AWS_SECRET_ACCESS_KEY="..." AWS_DEFAULT_REGION="..." restic snapshots --no-cache

# restore
env RESTIC_PASSWORD="..." RESTIC_REPOSITORY="s3:s3.amazonaws.com/bucketname/somepath" AWS_ACCESS_KEY_ID="..." AWS_SECRET_ACCESS_KEY="..." AWS_DEFAULT_REGION="..." restic restore --no-cache --target restorepath <the snapshot ID>

git -C restorepath/git-backup log

# convert bitwarden.sql to db.sqlite3
sqlite3 data/db.sqlite3 < restorepath/git-backup/bitwarden.sql
```

## Disaster Recovery Drill
