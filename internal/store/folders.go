package store

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	FolderMaxDepth      = 10
	FolderNameMaxRunes  = 80
	FolderMaxPerAccount = 500
)

func validateFolderName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", refusal("文件夹名称不能为空")
	}
	if !utf8.ValidString(name) {
		return "", refusal("文件夹名称必须是有效的 UTF-8 文本")
	}
	if utf8.RuneCountInString(name) > FolderNameMaxRunes {
		return "", refusalf("文件夹名称最长 %d 个字符", FolderNameMaxRunes)
	}
	if name == "." || name == ".." {
		return "", refusal("文件夹名称不能是 . 或 ..")
	}
	for _, r := range name {
		switch {
		case r == '/' || r == '\\':
			return "", refusal("文件夹名称不能包含路径分隔符")
		case unicode.IsControl(r):
			return "", refusal("文件夹名称不能包含控制字符")
		case unicode.Is(unicode.Bidi_Control, r):
			return "", refusal("文件夹名称不能包含文字方向控制符")
		}
	}
	return name, nil
}

// folderDepthTx returns the 1-based depth of folderID (1 = top-level folder),
// or ErrNotFound if folderID does not exist under ownerID.
func folderDepthTx(ctx context.Context, q storeQuerier, ownerID, folderID string) (int, error) {
	var depth int
	err := q.QueryRow(ctx, `
WITH RECURSIVE chain AS (
  SELECT id, parent_id, 1 AS depth
  FROM folders
  WHERE id = $1 AND owner_id = $2
  UNION ALL
  SELECT f.id, f.parent_id, c.depth + 1
  FROM folders f
  JOIN chain c ON f.id = c.parent_id
  WHERE f.owner_id = $2 AND c.depth < $3
)
SELECT MAX(depth) FROM chain
`, folderID, ownerID, FolderMaxDepth+2).Scan(&depth)
	if err != nil {
		return 0, translateNotFound(err)
	}
	if depth == 0 {
		return 0, ErrNotFound
	}
	return depth, nil
}

// CreateFolder creates a folder under parentID (or at the root when parentID is
// empty). Folder names are unique case-insensitively among siblings.
func (d *Store) CreateFolder(ctx context.Context, ownerID, parentID, rawName string) (Folder, error) {
	if !validUUIDs(ownerID) || ownerID == AnonymousUserID {
		return Folder{}, ErrNotFound
	}
	if parentID != "" && !validUUIDs(parentID) {
		return Folder{}, ErrNotFound
	}
	name, err := validateFolderName(rawName)
	if err != nil {
		return Folder{}, err
	}
	now := time.Now().UTC()
	folder := Folder{
		ID:        uuid.NewString(),
		OwnerID:   ownerID,
		ParentID:  parentID,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, ownerID).Scan(&locked); err != nil {
			return translateNotFound(err)
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM folders WHERE owner_id = $1`, ownerID).Scan(&count); err != nil {
			return fmt.Errorf("count folders: %w: %w", ErrInternal, err)
		}
		if count >= FolderMaxPerAccount {
			return refusalf("文件夹数量已达上限（最多 %d 个）", FolderMaxPerAccount)
		}
		if parentID != "" {
			depth, err := folderDepthTx(ctx, tx, ownerID, parentID)
			if err != nil {
				return err
			}
			if depth >= FolderMaxDepth {
				return refusalf("目录层级最多嵌套 %d 层", FolderMaxDepth)
			}
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO folders(id, owner_id, parent_id, name, created_at, updated_at)
VALUES($1, $2, NULLIF($3, '')::uuid, $4, $5, $5)
`, folder.ID, ownerID, parentID, name, now); err != nil {
			if isUniqueViolation(err) {
				return refusal("当前目录已存在同名文件夹")
			}
			return fmt.Errorf("create folder: %w: %w", ErrInternal, err)
		}
		return nil
	})
	if err != nil {
		return Folder{}, err
	}
	return folder, nil
}

// FolderForOwner reads one folder owned by ownerID.
func (d *Store) FolderForOwner(ctx context.Context, ownerID, folderID string) (Folder, error) {
	if !validUUIDs(ownerID, folderID) {
		return Folder{}, ErrNotFound
	}
	var f Folder
	err := d.db.QueryRow(ctx, `
SELECT id, owner_id, COALESCE(parent_id::text, ''), name, created_at, updated_at
FROM folders
WHERE id = $1 AND owner_id = $2
`, folderID, ownerID).Scan(&f.ID, &f.OwnerID, &f.ParentID, &f.Name, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return Folder{}, translateNotFound(err)
	}
	return f, nil
}

// FolderBreadcrumbs returns the path from the top-level ancestor down to
// folderID. An empty folderID returns nil, nil.
func (d *Store) FolderBreadcrumbs(ctx context.Context, ownerID, folderID string) ([]Folder, error) {
	if folderID == "" {
		return nil, nil
	}
	if !validUUIDs(ownerID, folderID) {
		return nil, ErrNotFound
	}
	rows, err := d.db.Query(ctx, `
WITH RECURSIVE chain AS (
  SELECT id, owner_id, parent_id, name, created_at, updated_at, 0 AS dist
  FROM folders
  WHERE id = $1 AND owner_id = $2
  UNION ALL
  SELECT f.id, f.owner_id, f.parent_id, f.name, f.created_at, f.updated_at, c.dist + 1
  FROM folders f
  JOIN chain c ON f.id = c.parent_id
  WHERE f.owner_id = $2 AND c.dist < $3
)
SELECT id, owner_id, COALESCE(parent_id::text, ''), name, created_at, updated_at
FROM chain
ORDER BY dist DESC
`, folderID, ownerID, FolderMaxDepth+2)
	if err != nil {
		return nil, fmt.Errorf("folder breadcrumbs: %w: %w", ErrInternal, err)
	}
	defer rows.Close()
	var crumbs []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.OwnerID, &f.ParentID, &f.Name, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("folder breadcrumbs: %w: %w", ErrInternal, err)
		}
		crumbs = append(crumbs, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(crumbs) == 0 {
		return nil, ErrNotFound
	}
	return crumbs, nil
}

// ListChildFolders returns the direct subfolders of parentID (empty = root),
// sorted by name, along with each folder's direct item count.
func (d *Store) ListChildFolders(ctx context.Context, ownerID, parentID string) ([]Folder, error) {
	if !validUUIDs(ownerID) {
		return nil, ErrNotFound
	}
	if parentID != "" && !validUUIDs(parentID) {
		return nil, nil
	}
	rows, err := d.db.Query(ctx, `
SELECT f.id, f.owner_id, COALESCE(f.parent_id::text, ''), f.name, f.created_at, f.updated_at,
       (SELECT COUNT(*) FROM folders cf WHERE cf.parent_id = f.id) +
       (SELECT COUNT(*) FROM resources r WHERE r.owner_id = f.owner_id AND r.folder_id = f.id AND `+liveResource+`)
FROM folders f
WHERE f.owner_id = $1 AND (($2 = '' AND f.parent_id IS NULL) OR ($2 <> '' AND f.parent_id = NULLIF($2, '')::uuid))
ORDER BY lower(f.name), f.name, f.id
`, ownerID, parentID)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w: %w", ErrInternal, err)
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.OwnerID, &f.ParentID, &f.Name, &f.CreatedAt, &f.UpdatedAt, &f.ItemCount); err != nil {
			return nil, fmt.Errorf("list folders: %w: %w", ErrInternal, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListAllFolders returns every folder belonging to ownerID, ordered by name,
// for building move dialogs and path lookups.
func (d *Store) ListAllFolders(ctx context.Context, ownerID string) ([]Folder, error) {
	if !validUUIDs(ownerID) {
		return nil, ErrNotFound
	}
	rows, err := d.db.Query(ctx, `
SELECT id, owner_id, COALESCE(parent_id::text, ''), name, created_at, updated_at
FROM folders
WHERE owner_id = $1
ORDER BY lower(name), name, id
`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list all folders: %w: %w", ErrInternal, err)
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.OwnerID, &f.ParentID, &f.Name, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list all folders: %w: %w", ErrInternal, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RenameFolder changes a folder's name within its current parent directory.
func (d *Store) RenameFolder(ctx context.Context, ownerID, folderID, rawName string) (Folder, error) {
	if !validUUIDs(ownerID, folderID) {
		return Folder{}, ErrNotFound
	}
	name, err := validateFolderName(rawName)
	if err != nil {
		return Folder{}, err
	}
	now := time.Now().UTC()
	var f Folder
	err = d.db.QueryRow(ctx, `
UPDATE folders
SET name = $3, updated_at = $4
WHERE id = $1 AND owner_id = $2
RETURNING id, owner_id, COALESCE(parent_id::text, ''), name, created_at, updated_at
`, folderID, ownerID, name, now).Scan(&f.ID, &f.OwnerID, &f.ParentID, &f.Name, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Folder{}, refusal("当前目录已存在同名文件夹")
		}
		return Folder{}, translateNotFound(err)
	}
	return f, nil
}

// MoveFolder moves folderID under targetParentID (empty = root), refusing
// cycles and depth overflows.
func (d *Store) MoveFolder(ctx context.Context, ownerID, folderID, targetParentID string) error {
	if !validUUIDs(ownerID, folderID) {
		return ErrNotFound
	}
	if targetParentID != "" && !validUUIDs(targetParentID) {
		return ErrNotFound
	}
	if folderID == targetParentID {
		return refusal("不能将文件夹移动到自身")
	}
	now := time.Now().UTC()
	return d.withTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, ownerID).Scan(&locked); err != nil {
			return translateNotFound(err)
		}
		var currentParent string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(parent_id::text, '') FROM folders WHERE id = $1 AND owner_id = $2`, folderID, ownerID).Scan(&currentParent); err != nil {
			return translateNotFound(err)
		}
		if currentParent == targetParentID {
			return nil
		}
		targetDepth := 0
		if targetParentID != "" {
			var err error
			targetDepth, err = folderDepthTx(ctx, tx, ownerID, targetParentID)
			if err != nil {
				return err
			}
			// Check whether targetParentID lies inside folderID's subtree.
			var inSubtree bool
			if err := tx.QueryRow(ctx, `
WITH RECURSIVE sub AS (
  SELECT id FROM folders WHERE id = $1 AND owner_id = $2
  UNION ALL
  SELECT f.id FROM folders f JOIN sub s ON f.parent_id = s.id WHERE f.owner_id = $2
)
SELECT EXISTS (SELECT 1 FROM sub WHERE id = $3)
`, folderID, ownerID, targetParentID).Scan(&inSubtree); err != nil {
				return fmt.Errorf("check folder cycle: %w: %w", ErrInternal, err)
			}
			if inSubtree {
				return refusal("不能将文件夹移动到其子目录中")
			}
		}
		// Compute height of folderID's subtree (1 if leaf).
		var height int
		if err := tx.QueryRow(ctx, `
WITH RECURSIVE sub AS (
  SELECT id, 1 AS h FROM folders WHERE id = $1 AND owner_id = $2
  UNION ALL
  SELECT f.id, s.h + 1 FROM folders f JOIN sub s ON f.parent_id = s.id WHERE f.owner_id = $2 AND s.h < $3
)
SELECT MAX(h) FROM sub
`, folderID, ownerID, FolderMaxDepth+2).Scan(&height); err != nil {
			return fmt.Errorf("folder height: %w: %w", ErrInternal, err)
		}
		if targetDepth+height > FolderMaxDepth {
			return refusalf("目录层级最多嵌套 %d 层", FolderMaxDepth)
		}
		if _, err := tx.Exec(ctx, `
UPDATE folders SET parent_id = NULLIF($3, '')::uuid, updated_at = $4
WHERE id = $1 AND owner_id = $2
`, folderID, ownerID, targetParentID, now); err != nil {
			if isUniqueViolation(err) {
				return refusal("目标目录已存在同名文件夹")
			}
			return fmt.Errorf("move folder: %w: %w", ErrInternal, err)
		}
		return nil
	})
}

// MoveResource moves a live resource into targetFolderID (empty = root).
func (d *Store) MoveResource(ctx context.Context, ownerID, resourceID, targetFolderID string) error {
	if !validUUIDs(ownerID, resourceID) {
		return ErrNotFound
	}
	if targetFolderID != "" && !validUUIDs(targetFolderID) {
		return ErrNotFound
	}
	return d.withTx(ctx, func(tx pgx.Tx) error {
		if targetFolderID != "" {
			var ok string
			if err := tx.QueryRow(ctx, `SELECT id FROM folders WHERE id = $1 AND owner_id = $2`, targetFolderID, ownerID).Scan(&ok); err != nil {
				return translateNotFound(err)
			}
		}
		tag, err := tx.Exec(ctx, `
UPDATE resources r
SET folder_id = NULLIF($3, '')::uuid
WHERE r.id = $1 AND r.owner_id = $2 AND `+liveResource, resourceID, ownerID, targetFolderID)
		if err != nil {
			return fmt.Errorf("move resource: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// TrashFolder removes folderID and all of its subfolders while moving every
// live resource inside that subtree to the trash and revoking their active
// share links (mirroring OS Finder/Explorer behavior). Trashed resources clear
// their folder_id so restoring them returns them cleanly to the root directory.
func (d *Store) TrashFolder(ctx context.Context, ownerID, folderID string, now time.Time) (trashedResources, revokedLinks int, err error) {
	if !validUUIDs(ownerID, folderID) {
		return 0, 0, ErrNotFound
	}
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, ownerID).Scan(&locked); err != nil {
			return translateNotFound(err)
		}
		var exists string
		if err := tx.QueryRow(ctx, `SELECT id FROM folders WHERE id = $1 AND owner_id = $2`, folderID, ownerID).Scan(&exists); err != nil {
			return translateNotFound(err)
		}
		subRows, err := tx.Query(ctx, `
WITH RECURSIVE sub AS (
  SELECT id FROM folders WHERE id = $1 AND owner_id = $2
  UNION ALL
  SELECT f.id FROM folders f JOIN sub s ON f.parent_id = s.id WHERE f.owner_id = $2
)
SELECT id::text FROM sub
`, folderID, ownerID)
		if err != nil {
			return fmt.Errorf("folder subtree: %w: %w", ErrInternal, err)
		}
		folderIDs, err := pgx.CollectRows(subRows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("folder subtree: %w: %w", ErrInternal, err)
		}
		resRows, err := tx.Query(ctx, `
SELECT id::text FROM resources
WHERE owner_id = $1 AND folder_id = ANY($2::uuid[])
  AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at > $3)
FOR UPDATE
`, ownerID, folderIDs, now)
		if err != nil {
			return fmt.Errorf("lock folder resources: %w: %w", ErrInternal, err)
		}
		resIDs, err := pgx.CollectRows(resRows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("lock folder resources: %w: %w", ErrInternal, err)
		}
		if len(resIDs) > 0 {
			if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM links
WHERE resource_id = ANY($1::uuid[]) AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > $2)
  AND (max_uses = 0 OR used_count < max_uses)
`, resIDs, now).Scan(&revokedLinks); err != nil {
				return fmt.Errorf("count live shares: %w: %w", ErrInternal, err)
			}
			if _, err := tx.Exec(ctx, `
UPDATE links SET revoked_at = $1
WHERE resource_id = ANY($2::uuid[]) AND revoked_at IS NULL
`, now, resIDs); err != nil {
				return fmt.Errorf("revoke folder shares: %w: %w", ErrInternal, err)
			}
			tag, err := tx.Exec(ctx, `
UPDATE resources SET deleted_at = $1, folder_id = NULL
WHERE id = ANY($2::uuid[]) AND owner_id = $3
`, now, resIDs, ownerID)
			if err != nil {
				return fmt.Errorf("trash folder resources: %w: %w", ErrInternal, err)
			}
			trashedResources = int(tag.RowsAffected())
		}
		if _, err := tx.Exec(ctx, `
UPDATE resources SET folder_id = NULL
WHERE owner_id = $1 AND folder_id = ANY($2::uuid[])
`, ownerID, folderIDs); err != nil {
			return fmt.Errorf("detach folder resources: %w: %w", ErrInternal, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM folders WHERE id = $1 AND owner_id = $2`, folderID, ownerID); err != nil {
			return fmt.Errorf("delete folder: %w: %w", ErrInternal, err)
		}
		return nil
	})
	return trashedResources, revokedLinks, err
}
