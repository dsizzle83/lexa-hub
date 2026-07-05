package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Scan iterates every rotated file for the journal named dir/name, oldest to
// newest (name.N .. name.1, then the active file name), calling fn once per
// successfully-decoded Event in write order.
//
// A line that fails to parse as an Event is tolerated and counted rather
// than treated as fatal: by construction (Append writes one line per
// os.File.Write call, and a rotated file is fsynced and closed before it is
// ever renamed — never appended to again) the only line that can be torn is
// the final line of the currently-active file, from a power cut mid-write.
// Scan does not special-case "only the last line" for this tolerance —
// it counts and skips any unparseable line, wherever it occurs — so a
// caller doesn't need to reason about which file is "the last one" itself.
//
// Scan stops and returns fn's error immediately if fn returns non-nil,
// alongside the partial-skip count accumulated so far. It also stops and
// returns an error if the directory or a file cannot be read (a real I/O
// failure, distinct from a decode failure).
func Scan(dir, name string, fn func(Event) error) (skipped int, err error) {
	files, err := orderedFiles(dir, name)
	if err != nil {
		return 0, fmt.Errorf("journal: list %s/%s*: %w", dir, name, err)
	}
	for _, path := range files {
		n, err := scanFile(path, fn)
		skipped += n
		if err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}

// orderedFiles lists the on-disk files for one journal name, oldest first:
// name.N, name.(N-1), ..., name.1, then the active name (if present). N is
// discovered from whatever ".<int>" suffixes actually exist on disk — Scan
// does not need to know the Writer's MaxFiles to read what it wrote.
func orderedFiles(dir, name string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	prefix := name + "."
	type rotated struct {
		n    int
		path string
	}
	var files []rotated
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		nm := e.Name()
		if !strings.HasPrefix(nm, prefix) {
			continue
		}
		n, err := strconv.Atoi(nm[len(prefix):])
		if err != nil {
			continue // not one of our rotation files (e.g. an unrelated dotted name)
		}
		files = append(files, rotated{n, filepath.Join(dir, nm)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].n > files[j].n }) // oldest (highest N) first

	out := make([]string, 0, len(files)+1)
	for _, f := range files {
		out = append(out, f.path)
	}
	active := filepath.Join(dir, name)
	if _, err := os.Stat(active); err == nil {
		out = append(out, active)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

// scanFile decodes one NDJSON file, returning the count of lines that failed
// to parse (skipped, not fatal) and any real read error.
func scanFile(path string, fn func(Event) error) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20) // allow generously large single lines

	skipped := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			skipped++ // truncated/corrupt line (e.g. a torn final write): tolerate
			continue
		}
		if err := fn(e); err != nil {
			return skipped, err
		}
	}
	if err := sc.Err(); err != nil {
		return skipped, err
	}
	return skipped, nil
}
