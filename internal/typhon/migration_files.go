package typhon

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const migrationFileLimit = 1 << 20
const migrationNonTransactionalMarker = "-- typhon:no-transaction"

var migrationFilename = regexp.MustCompile(`^([0-9]{6})_([a-z][a-z0-9_]{0,62})\.(up|down)\.sql$`)
var migrationConcurrentIndex = regexp.MustCompile(`(?i)^(CREATE\s+(UNIQUE\s+)?INDEX\s+CONCURRENTLY|DROP\s+INDEX\s+CONCURRENTLY)\b`)

type migrationFile struct {
	sql      string
	checksum string
	nonTx    bool
}

type migrationVersion struct {
	version int64
	name    string
	up      *migrationFile
	down    *migrationFile
}

func loadMigrationFiles(directory string) ([]migrationVersion, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	versions := make(map[int64]*migrationVersion)
	totalBytes := 0
	for {
		entries, readErr := dir.ReadDir(64)
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".sql") {
				continue
			}
			match := migrationFilename.FindStringSubmatch(entry.Name())
			if match == nil || !entry.Type().IsRegular() {
				return nil, fmt.Errorf("invalid migration filename or file type %q", entry.Name())
			}
			ver, _ := strconv.ParseInt(match[1], 10, 64)
			if ver == 0 {
				return nil, errors.New("migration versions start at 000001")
			}
			version := versions[ver]
			if version == nil {
				if len(versions) >= 4096 {
					return nil, errors.New("migration directory exceeds 4096 versions")
				}
				version = &migrationVersion{version: ver, name: match[2]}
				versions[ver] = version
			}
			if version.name != match[2] {
				return nil, fmt.Errorf("duplicate migration version %06d", ver)
			}
			file, err := readMigrationFile(root, entry.Name())
			if err != nil {
				return nil, fmt.Errorf("%s: %w", entry.Name(), err)
			}
			totalBytes += len(file.sql)
			if totalBytes > 32<<20 {
				return nil, errors.New("migration plan exceeds 32 MiB")
			}
			if match[3] == "up" {
				version.up = file
			} else {
				version.down = file
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, readErr
		}
	}
	out := make([]migrationVersion, 0, len(versions))
	for _, version := range versions {
		out = append(out, *version)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, version := range out {
		if version.version != int64(i+1) || version.up == nil {
			return nil, fmt.Errorf("expected contiguous up migration %06d", i+1)
		}
	}
	return out, nil
}

func readMigrationFile(root *os.Root, path string) (*migrationFile, error) {
	file, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, migrationFileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > migrationFileLimit || !utf8.Valid(raw) || bytes.IndexByte(raw, 0) != -1 {
		return nil, errors.New("migration SQL must be UTF-8, without NUL and at most 1 MiB")
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	sql := string(raw)
	header, _, _ := strings.Cut(strings.TrimSpace(sql), "\n")
	nonTx := strings.TrimSpace(header) == migrationNonTransactionalMarker
	statement := migrationWithoutLeadingComments(sql)
	cut := strings.IndexFunc(statement, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z')
	})
	verb := statement
	if cut >= 0 {
		verb = verb[:cut]
	}
	switch strings.ToUpper(verb) {
	case "CREATE", "ALTER", "DROP", "COMMENT", "GRANT", "REVOKE",
		"INSERT", "UPDATE", "DELETE", "MERGE", "DO":
	default:
		return nil, errors.New("migration requires a supported DDL or data statement")
	}
	concurrently := migrationConcurrentIndex.MatchString(statement)
	if nonTx != concurrently {
		return nil, errors.New("use the no-transaction marker only for CREATE or DROP INDEX CONCURRENTLY")
	}
	return &migrationFile{sql: sql, checksum: fmt.Sprintf("%x", sha256.Sum256(raw)), nonTx: nonTx}, nil
}

func migrationWithoutLeadingComments(sql string) string {
	for {
		sql = strings.TrimSpace(sql)
		if strings.HasPrefix(sql, "--") {
			_, sql, _ = strings.Cut(sql, "\n")
			continue
		}
		if !strings.HasPrefix(sql, "/*") {
			return sql
		}
		depth, index := 1, 2
		for depth != 0 && index+1 < len(sql) {
			switch sql[index : index+2] {
			case "/*":
				depth++
				index += 2
			case "*/":
				depth--
				index += 2
			default:
				index++
			}
		}
		if depth != 0 {
			return ""
		}
		sql = sql[index:]
	}
}
