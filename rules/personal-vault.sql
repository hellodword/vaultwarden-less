-- Vaultwarden 1.37.5, src/db/schema.rs and src/api/core/{accounts,ciphers,folders}.rs.
-- Inputs are two standalone, immutable SQLite databases. Return 1 for a change.
-- Server-side password verification, device state and revision timestamps can
-- change on login or sync. The account key and client KDF unlock personal data.
WITH
old_accounts AS (
    SELECT uuid, email, akey, key_id, private_key, public_key,
           client_kdf_type, client_kdf_iter, client_kdf_memory, client_kdf_parallelism
    FROM previous.users
),
new_accounts AS (
    SELECT uuid, email, akey, key_id, private_key, public_key,
           client_kdf_type, client_kdf_iter, client_kdf_memory, client_kdf_parallelism
    FROM current.users
),
old_items AS (
    SELECT uuid, user_uuid, created_at, key, atype, name, notes, fields,
           data, password_history, deleted_at, reprompt
    FROM previous.ciphers WHERE user_uuid IS NOT NULL AND organization_uuid IS NULL
),
new_items AS (
    SELECT uuid, user_uuid, created_at, key, atype, name, notes, fields,
           data, password_history, deleted_at, reprompt
    FROM current.ciphers WHERE user_uuid IS NOT NULL AND organization_uuid IS NULL
),
old_folders AS (SELECT uuid, user_uuid, name FROM previous.folders),
new_folders AS (SELECT uuid, user_uuid, name FROM current.folders),
old_membership AS (
    SELECT i.uuid AS cipher_uuid, f.uuid AS folder_uuid FROM previous.folders_ciphers m
    JOIN old_items i ON i.uuid = m.cipher_uuid
    JOIN old_folders f ON f.uuid = m.folder_uuid AND f.user_uuid = i.user_uuid
),
new_membership AS (
    SELECT i.uuid AS cipher_uuid, f.uuid AS folder_uuid FROM current.folders_ciphers m
    JOIN new_items i ON i.uuid = m.cipher_uuid
    JOIN new_folders f ON f.uuid = m.folder_uuid AND f.user_uuid = i.user_uuid
),
old_favorites AS (
    SELECT i.uuid FROM previous.favorites f
    JOIN old_items i ON i.uuid = f.cipher_uuid AND i.user_uuid = f.user_uuid
),
new_favorites AS (
    SELECT i.uuid FROM current.favorites f
    JOIN new_items i ON i.uuid = f.cipher_uuid AND i.user_uuid = f.user_uuid
),
old_archives AS (
    SELECT i.uuid, a.archived_at FROM previous.archives a
    JOIN old_items i ON i.uuid = a.cipher_uuid AND i.user_uuid = a.user_uuid
),
new_archives AS (
    SELECT i.uuid, a.archived_at FROM current.archives a
    JOIN new_items i ON i.uuid = a.cipher_uuid AND i.user_uuid = a.user_uuid
)
SELECT EXISTS (SELECT * FROM old_accounts EXCEPT SELECT * FROM new_accounts)
    OR EXISTS (SELECT * FROM new_accounts EXCEPT SELECT * FROM old_accounts)
    OR EXISTS (SELECT * FROM old_items EXCEPT SELECT * FROM new_items)
    OR EXISTS (SELECT * FROM new_items EXCEPT SELECT * FROM old_items)
    OR EXISTS (SELECT * FROM old_folders EXCEPT SELECT * FROM new_folders)
    OR EXISTS (SELECT * FROM new_folders EXCEPT SELECT * FROM old_folders)
    OR EXISTS (SELECT * FROM old_membership EXCEPT SELECT * FROM new_membership)
    OR EXISTS (SELECT * FROM new_membership EXCEPT SELECT * FROM old_membership)
    OR EXISTS (SELECT * FROM old_favorites EXCEPT SELECT * FROM new_favorites)
    OR EXISTS (SELECT * FROM new_favorites EXCEPT SELECT * FROM old_favorites)
    OR EXISTS (SELECT * FROM old_archives EXCEPT SELECT * FROM new_archives)
    OR EXISTS (SELECT * FROM new_archives EXCEPT SELECT * FROM old_archives);
