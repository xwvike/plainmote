package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsRefusal(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{refusal("文件名不能包含控制字符"), true},
		{fmt.Errorf("save: %w", ErrTakenDown), true},
		{storageQuotaError(10, 9, 5), true},
		{&VersionConflict{Current: 3}, true},
		{ErrNotFound, true},
		{validateFilename(".."), true},
		{errors.New("failed to connect to host=postgres user=plainmote database=plainmote"), false},
		{fmt.Errorf("update share: %w", errors.New("conn closed")), false},
		{fmt.Errorf("read: %w: %w", ErrInternal, errors.New("pgx")), false},
		{context.Canceled, false},
	} {
		if got := IsRefusal(tc.err); got != tc.want {
			t.Errorf("IsRefusal(%v) = %v", tc.err, got)
		}
	}
}
