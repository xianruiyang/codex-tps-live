package codextps

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Only typed request protocol observations are retained. No stderr/body/header,
// token content or TTFT estimate is copied into this index. Each monitor owns
// its writer file; multiple local monitor instances do not share an appender.
type liveTransportCache struct {
	dir, prefix, name string
	loaded            bool
	nextLoad          time.Time
	seen              map[string]bool
}

type liveTransportRecord struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Process string    `json:"process"`
	Thread  string    `json:"thread"`
	Turn    string    `json:"turn"`
	Kind    string    `json:"kind"`
}

func timingEventKey(e liveTimingEvent) string {
	return fmt.Sprintf("%s|%d|%s|%s|%s", e.process, e.id, e.at.UTC().Format(time.RFC3339Nano), e.thread, e.kind)
}

func protocolObservation(kind string) bool {
	return kind == "websocket_request" || kind == "http_request" || kind == "mixed_request" || kind == "http_fallback"
}

func (c *liveTransportCache) sync(home string, events []liveTimingEvent) ([]liveTimingEvent, error) {
	var retained []liveTimingEvent
	var firstErr error
	if c.dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(home))))
		c.dir = filepath.Join(base, "CodexTPSLive", "protocol")
		c.prefix = "protocol-v1-" + hex.EncodeToString(digest[:8]) + "-"
		c.name = c.prefix + time.Now().UTC().Format("20060102T150405.000000000") + fmt.Sprintf("-%d.jsonl", os.Getpid())
		c.seen = map[string]bool{}
	}
	if !c.loaded && !time.Now().Before(c.nextLoad) {
		retained, c.loaded, firstErr = c.load()
		c.nextLoad = time.Now().Add(5 * time.Second)
	}
	for _, e := range events {
		if !protocolObservation(e.kind) || e.thread == "" || e.turn == "" {
			continue
		}
		key := timingEventKey(e)
		if c.seen[key] {
			continue
		}
		if err := c.append(e); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		c.seen[key] = true
	}
	return retained, firstErr
}

func (c *liveTransportCache) load() ([]liveTimingEvent, bool, error) {
	entries, err := os.ReadDir(c.dir)
	if os.IsNotExist(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	type cacheFile struct {
		name string
		info os.FileInfo
	}
	var files []cacheFile
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, c.prefix) && (strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.previous")) {
			if info, err := entry.Info(); err == nil {
				files = append(files, cacheFile{name, info})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].info.ModTime().After(files[j].info.ModTime()) })
	var result []liveTimingEvent
	invalid := 0
	// Restore the newest bounded metadata, not unlimited historical log files.
	budget := int64(4 * 1024 * 1024)
	for _, file := range files {
		if budget <= 0 {
			break
		}
		f, err := os.Open(filepath.Join(c.dir, file.name))
		if err != nil {
			return result, false, err
		}
		length := min(file.info.Size(), budget)
		start := file.info.Size() - length
		reader := io.NewSectionReader(f, start, length)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 16*1024)
		if start > 0 {
			var previous [1]byte
			if _, err := f.ReadAt(previous[:], start-1); err == nil && previous[0] != '\n' {
				scanner.Scan() // discard a partial first record
			}
		}
		for scanner.Scan() {
			var record liveTransportRecord
			if json.Unmarshal(scanner.Bytes(), &record) != nil {
				invalid++
				continue
			}
			if record.At.IsZero() || record.Thread == "" || record.Turn == "" || len(record.Process) > 64 || len(record.Thread) > 64 || len(record.Turn) > 64 || !protocolObservation(record.Kind) {
				invalid++
				continue
			}
			e := liveTimingEvent{id: record.ID, at: record.At, process: record.Process, thread: record.Thread, turn: record.Turn, kind: record.Kind, cached: true}
			key := timingEventKey(e)
			if !c.seen[key] {
				result = append(result, e)
				c.seen[key] = true
			}
		}
		scanErr := scanner.Err()
		f.Close()
		if scanErr != nil {
			return result, false, scanErr
		}
		budget -= length
	}
	if invalid > 0 {
		return result, true, fmt.Errorf("协议索引中有 %d 条损坏或不支持的记录，已保留其余有效证据", invalid)
	}
	return result, true, nil
}

func (c *liveTransportCache) append(e liveTimingEvent) error {
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(c.dir, c.name)
	if info, err := os.Stat(path); err == nil && info.Size() >= 1024*1024 {
		if err := os.Remove(path + ".previous"); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(path, path+".previous"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(liveTransportRecord{e.id, e.at, e.process, e.thread, e.turn, e.kind})
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
