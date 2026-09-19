package web

import (
	"errors"
	"strconv"
	"strings"
)

var errInvalidRange = errors.New("invalid byte range")

type byteRange struct {
	start int64
	end   int64
}

func (r byteRange) length() int64 { return r.end - r.start + 1 }

// parseByteRange accepts the single range browsers use for media and ordinary
// resumable downloads. Multipart ranges are refused rather than assembled in
// memory. An unknown range unit is ignored as required for a normal full-body
// response.
func parseByteRange(header string, size int64) (*byteRange, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, nil
	}
	unit, value, found := strings.Cut(header, "=")
	if !found || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return nil, nil
	}
	value = strings.TrimSpace(value)
	if size <= 0 || value == "" || strings.Contains(value, ",") {
		return nil, errInvalidRange
	}
	first, last, found := strings.Cut(value, "-")
	if !found {
		return nil, errInvalidRange
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)
	if first == "" {
		suffix, err := strconv.ParseInt(last, 10, 64)
		if err != nil || suffix <= 0 {
			return nil, errInvalidRange
		}
		if suffix > size {
			suffix = size
		}
		return &byteRange{start: size - suffix, end: size - 1}, nil
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= size {
		return nil, errInvalidRange
	}
	end := size - 1
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return nil, errInvalidRange
		}
		if end >= size {
			end = size - 1
		}
	}
	return &byteRange{start: start, end: end}, nil
}
