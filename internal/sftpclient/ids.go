package sftpclient

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"sync"
)

// maxIDFile caps how much of the account files we read, so an unexpected
// response cannot make us buffer without bound.
const maxIDFile = 1 << 20 // 1 MiB

// passwdPath and groupPath are read over the same SFTP session to turn numeric
// ids into names.
const (
	passwdPath = "/etc/passwd"
	groupPath  = "/etc/group"
)

// parseIDLine extracts the id and name from one colon-separated record of
// /etc/passwd or /etc/group. Both files start with name:password:id:, so the
// same layout applies, and only the first three fields are ever needed.
//
// It reports false for anything that is not a plain account line: truncated
// records, NIS placeholders ("+user") and non-numeric ids.
func parseIDLine(line string) (id string, name string, ok bool) {
	fields := strings.Split(strings.TrimSpace(line), ":")
	if len(fields) < 3 {
		return "", "", false
	}
	name = fields[0]
	if name == "" || strings.HasPrefix(name, "+") || strings.HasPrefix(name, "-") {
		return "", "", false
	}
	if _, err := strconv.ParseUint(fields[2], 10, 32); err != nil {
		return "", "", false
	}
	return fields[2], name, true
}

// idMaps resolves numeric uid/gid to account names.
//
// The SFTP protocol only carries owner/group *names* in the "longname" field of
// a name reply, and pkg/sftp's client discards it, so the raw attributes give us
// numbers alone. Reading the account files over the existing session is the way
// to show names without executing anything on the server. Accounts served by
// LDAP/NIS are absent from those files and fall back to their decimal id.
type idMaps struct {
	once   sync.Once
	c      *Client
	users  map[string]string
	groups map[string]string
}

// readIDNames loads one account file into an id -> name map. A missing or
// unreadable file yields an empty map, which simply leaves every id numeric.
func (m *idMaps) readIDNames(path string) map[string]string {
	out := map[string]string{}
	f, err := m.c.sftp.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()

	sc := bufio.NewScanner(io.LimitReader(f, maxIDFile))
	for sc.Scan() {
		id, name, ok := parseIDLine(sc.Text())
		if !ok {
			continue
		}
		out[id] = name
	}
	// Scan cannot tell a clean end of file from a torn read; only Err() can.
	// Without this check a half-read file would stay cached for the whole
	// session (load runs once), so some ids would show names and others numbers
	// for no visible reason. A decimal id is always truthful, so discard the
	// partial map instead of keeping a result we cannot trust to be complete.
	if err := sc.Err(); err != nil {
		return map[string]string{}
	}
	return out
}

func (m *idMaps) load() {
	m.users = m.readIDNames(passwdPath)
	m.groups = m.readIDNames(groupPath)
}

// lookupName resolves id through table, falling back to the decimal text of id.
func lookupName(table map[string]string, id uint32) string {
	key := strconv.FormatUint(uint64(id), 10)
	if n, ok := table[key]; ok {
		return n
	}
	return key
}

func (m *idMaps) user(uid uint32) string {
	m.once.Do(m.load)
	return lookupName(m.users, uid)
}

func (m *idMaps) group(gid uint32) string {
	m.once.Do(m.load)
	return lookupName(m.groups, gid)
}
