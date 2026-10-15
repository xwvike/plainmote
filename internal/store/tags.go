package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// TagMaxRunes bounds one tag. How many a resource carries is not bounded.
const TagMaxRunes = 24

// Tag is one of an account's tags, with how many of its resources carry it.
type Tag struct {
	Name  string
	Count int
}

// NormalizeTag trims a tag and refuses what cannot be shown as one: the same
// control and direction characters a filename may not hold.
func NormalizeTag(tag string) (string, error) {
	tag = strings.TrimSpace(tag)
	switch {
	case tag == "":
		return "", refusal("标签不能为空")
	case !utf8.ValidString(tag):
		return "", refusal("标签必须是有效的 UTF-8 文本")
	case utf8.RuneCountInString(tag) > TagMaxRunes:
		return "", refusalf("标签最长 %d 个字符", TagMaxRunes)
	}
	for _, r := range tag {
		switch {
		case unicode.IsControl(r):
			return "", refusal("标签不能包含控制字符")
		case unicode.Is(unicode.Bidi_Control, r):
			return "", refusal("标签不能包含文字方向控制符")
		}
	}
	return tag, nil
}

// SetResourceTags replaces a resource's tags. A tag the account already uses
// in another case takes that spelling, so one tag never shows as two. Tags
// are labels, not content: a quick share or an encrypted resource takes them
// too, and the resource does not count as updated. A tag it carries already
// is kept as it is, so a tag from under an older, longer limit stays.
func (d *Store) SetResourceTags(ctx context.Context, ownerID, id string, tags []string) ([]string, error) {
	current, err := d.ResourceForOwner(ctx, ownerID, id)
	if err != nil {
		return nil, err
	}
	byKey := map[string]string{}
	keys := make([]string, 0, len(tags))
	for _, tag := range tags {
		if !slices.Contains(current.Tags, tag) {
			if tag, err = NormalizeTag(tag); err != nil {
				return nil, err
			}
		}
		key := strings.ToLower(tag)
		if _, seen := byKey[key]; !seen {
			byKey[key] = tag
			keys = append(keys, key)
		}
	}
	var saved []string
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, ownerID).Scan(&locked); err != nil {
			return translateNotFound(err)
		}
		rows, err := tx.Query(ctx, `
SELECT DISTINCT ON (lower(t)) t
FROM resources r, unnest(r.tags) t
WHERE r.owner_id = $1 AND r.id <> $2 AND lower(t) = ANY($3)
ORDER BY lower(t), t
`, ownerID, id, keys)
		if err != nil {
			return fmt.Errorf("read tags: %w: %w", ErrInternal, err)
		}
		used, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("read tags: %w: %w", ErrInternal, err)
		}
		for _, tag := range used {
			byKey[strings.ToLower(tag)] = tag
		}
		saved = make([]string, 0, len(keys))
		for _, key := range keys {
			saved = append(saved, byKey[key])
		}
		sortTags(saved)
		result, err := tx.Exec(ctx, `UPDATE resources r SET tags = $3 WHERE r.id = $1 AND r.owner_id = $2 AND `+liveResource,
			id, ownerID, saved)
		if err != nil {
			return fmt.Errorf("save tags: %w: %w", ErrInternal, err)
		}
		if result.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// AccountTags is every tag on the account's resources, by name, with how
// many carry each.
func (d *Store) AccountTags(ctx context.Context, ownerID string) ([]Tag, error) {
	if !validUUIDs(ownerID) {
		return nil, ErrNotFound
	}
	rows, err := d.db.Query(ctx, `
SELECT min(t), COUNT(*)
FROM resources r, unnest(r.tags) t
WHERE r.owner_id = $1 AND `+liveResource+`
GROUP BY lower(t)
`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w: %w", ErrInternal, err)
	}
	tags, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Tag, error) {
		var tag Tag
		err := row.Scan(&tag.Name, &tag.Count)
		return tag, err
	})
	if err != nil {
		return nil, fmt.Errorf("list tags: %w: %w", ErrInternal, err)
	}
	slices.SortFunc(tags, func(a, b Tag) int { return compareTags(a.Name, b.Name) })
	return tags, nil
}

func sortTags(tags []string) { slices.SortFunc(tags, compareTags) }

func compareTags(a, b string) int {
	if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}
