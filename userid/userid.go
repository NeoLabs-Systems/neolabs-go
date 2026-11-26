// Package userid is the account id shared packages store and pass around.
// NeoAgent numbers its accounts and NeoRecall names them with UUIDs; both fit.
package userid

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// ID is an account id in text form. The zero value means no account.
type ID string

// FromInt is the id of a numbered account.
func FromInt(n int64) ID { return ID(strconv.FormatInt(n, 10)) }

// Int64 is a numbered account's id, or an error for any other kind.
func (id ID) Int64() (int64, error) {
	n, err := strconv.ParseInt(string(id), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("userid: %q is not a numbered account", string(id))
	}
	return n, nil
}

// MarshalJSON writes a numbered id as a JSON number, which is what clients of
// the numbered apps expect, and any other id as a string.
func (id ID) MarshalJSON() ([]byte, error) {
	if n, err := id.Int64(); err == nil && FromInt(n) == id {
		return []byte(id), nil
	}
	return json.Marshal(string(id))
}

// Scan reads the id from an INTEGER or TEXT column.
func (id *ID) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*id = ""
	case int64:
		*id = FromInt(v)
	case string:
		*id = ID(v)
	case []byte:
		*id = ID(v)
	default:
		return fmt.Errorf("userid: cannot read %T", src)
	}
	return nil
}
