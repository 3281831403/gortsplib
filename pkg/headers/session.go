package headers

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bluenviron/gortsplib/v5/pkg/base"
)

// Session is a Session header.
type Session struct {
	// session id
	Session string

	// (optional) timeout
	Timeout *uint
}

// Unmarshal decodes a Session header.
func (h *Session) Unmarshal(v base.HeaderValue) error {
	if len(v) == 0 {
		return fmt.Errorf("value not provided")
	}

	if len(v) > 1 {
		return fmt.Errorf("value provided multiple times (%v)", v)
	}

	// tolerate leading and trailing spaces, that are sent by some cameras
	v0 := strings.TrimSpace(v[0])

	i := strings.IndexByte(v0, ';')
	if i < 0 {
		h.Session = v0
	} else {
		h.Session = strings.TrimSpace(v0[:i])
		v0 = strings.TrimLeft(v0[i+1:], " ")

		kvs, err := keyValParse(v0, ';')
		if err != nil {
			return err
		}

		for k, v := range kvs {
			if k == "timeout" {
				var iv uint64
				iv, err = strconv.ParseUint(v, 10, 32)
				if err != nil {
					return err
				}
				uiv := uint(iv)
				h.Timeout = &uiv
			}
		}
	}

	if h.Session == "" {
		return fmt.Errorf("session id not provided")
	}

	return nil
}

// Marshal encodes a Session header.
func (h Session) Marshal() base.HeaderValue {
	ret := h.Session

	if h.Timeout != nil {
		ret += ";timeout=" + strconv.FormatUint(uint64(*h.Timeout), 10)
	}

	return base.HeaderValue{ret}
}
