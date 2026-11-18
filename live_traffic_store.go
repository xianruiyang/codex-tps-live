package codextps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

type liveTrafficStore struct {
	db      *sql.DB
	release func()
}

func openLiveTrafficStore(home, dataDir string) (*liveTrafficStore, map[string]liveTrafficCheckpoint, liveTrafficStatus, error) {
	status := liveTrafficStatus{PollSeconds: 1}
	checkpoints := map[string]liveTrafficCheckpoint{}
	if dataDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, checkpoints, status, errors.New("无法定位流量数据目录")
		}
		dataDir = filepath.Join(base, "CodexTPSLive")
	}
	dir := filepath.Join(dataDir, "traffic", proxyHash(filepath.Clean(home)))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, checkpoints, status, errors.New("无法创建流量数据目录")
	}
	release, err := lockLiveNodePlans(filepath.Join(dir, "collector.lock"))
	if err != nil {
		return nil, checkpoints, status, errors.New("流量记录目录已由另一监控实例占用；本实例不重复采集")
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "history.sqlite"))
	if err != nil {
		release()
		return nil, checkpoints, status, errors.New("无法打开流量数据库")
	}
	store := &liveTrafficStore{db: db, release: release}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=1500; PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;
CREATE TABLE IF NOT EXISTS traffic_meta(key TEXT PRIMARY KEY,data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS traffic_connections(id TEXT PRIMARY KEY,first_seen INTEGER NOT NULL,last_seen INTEGER NOT NULL,data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS traffic_connections_seen ON traffic_connections(last_seen);
CREATE TABLE IF NOT EXISTS traffic_sessions(id TEXT PRIMARY KEY,data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS traffic_samples(at INTEGER NOT NULL,connection TEXT NOT NULL,session TEXT NOT NULL,upload INTEGER NOT NULL,download INTEGER NOT NULL,gap_upload INTEGER NOT NULL,gap_download INTEGER NOT NULL,samples INTEGER NOT NULL,PRIMARY KEY(at,connection,session));`)
	if err != nil {
		store.close()
		return nil, checkpoints, status, errors.New("无法初始化流量数据库")
	}
	if err = store.migrateAttribution(); err != nil {
		store.close()
		return nil, checkpoints, status, errors.New("无法校正流量分摊记录")
	}
	var data []byte
	err = db.QueryRow("SELECT data FROM traffic_meta WHERE key='status'").Scan(&data)
	if err == nil {
		err = json.Unmarshal(data, &status)
	}
	if err != nil && err != sql.ErrNoRows {
		store.close()
		return nil, checkpoints, status, errors.New("流量采集检查点无法读取")
	}
	status.Running, status.Error, status.PollSeconds = false, "", 1
	rows, err := db.Query("SELECT data FROM traffic_connections WHERE json_extract(data,'$.connection.active')=1")
	if err != nil {
		store.close()
		return nil, checkpoints, status, errors.New("流量连接检查点无法读取")
	}
	for rows.Next() {
		var checkpoint liveTrafficCheckpoint
		if err = rows.Scan(&data); err != nil {
			break
		}
		if err = json.Unmarshal(data, &checkpoint); err != nil {
			break
		}
		checkpoints[checkpoint.Connection.ID] = checkpoint
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		store.close()
		return nil, checkpoints, status, errors.New("流量连接检查点无法解析")
	}
	return store, checkpoints, status, nil
}

func (s *liveTrafficStore) migrateAttribution() error {
	var version string
	err := s.db.QueryRow("SELECT data FROM traffic_meta WHERE key='attribution_version'").Scan(&version)
	if err == nil && version == "1" {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Early local recordings matched all OpenAI subdomains. Keep their bytes,
	// but move background/desktop connection estimates to unallocated atomically.
	const ineligible = `NOT (lower(COALESCE(json_extract(c.data,'$.connection.process'),''))='codex.exe'
AND lower(rtrim(COALESCE(json_extract(c.data,'$.connection.host'),''),'.')) IN ('chatgpt.com','api.openai.com')
AND lower(COALESCE(json_extract(c.data,'$.connection.network'),''))='tcp')`
	_, err = tx.Exec(`INSERT INTO traffic_samples(at,connection,session,upload,download,gap_upload,gap_download,samples)
SELECT s.at,s.connection,'',SUM(s.upload),SUM(s.download),SUM(s.gap_upload),SUM(s.gap_download),SUM(s.samples)
FROM traffic_samples s JOIN traffic_connections c ON c.id=s.connection WHERE s.session<>'' AND ` + ineligible + ` GROUP BY s.at,s.connection
ON CONFLICT(at,connection,session) DO UPDATE SET upload=upload+excluded.upload,download=download+excluded.download,gap_upload=gap_upload+excluded.gap_upload,gap_download=gap_download+excluded.gap_download,samples=samples+excluded.samples`)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM traffic_samples WHERE session<>'' AND connection IN (SELECT c.id FROM traffic_connections c WHERE ` + ineligible + `)`); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO traffic_meta(key,data) VALUES('attribution_version','1') ON CONFLICT(key) DO UPDATE SET data=excluded.data"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *liveTrafficStore) close() {
	s.db.Close()
	s.release()
}

func (s *liveTrafficStore) checkpoint(ctx context.Context, id string) (liveTrafficCheckpoint, bool, error) {
	var checkpoint liveTrafficCheckpoint
	var data []byte
	err := s.db.QueryRowContext(ctx, "SELECT data FROM traffic_connections WHERE id=?", id).Scan(&data)
	if err == sql.ErrNoRows {
		return checkpoint, false, nil
	}
	if err != nil {
		return checkpoint, false, err
	}
	if err = json.Unmarshal(data, &checkpoint); err != nil {
		return checkpoint, false, err
	}
	return checkpoint, true, nil
}

func (s *liveTrafficStore) save(ctx context.Context, changed map[string]liveTrafficCheckpoint, deltas []liveTrafficDelta, candidates []liveTrafficCandidate, status liveTrafficStatus) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	connectionStmt, err := tx.PrepareContext(ctx, "INSERT INTO traffic_connections(id,first_seen,last_seen,data) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET first_seen=excluded.first_seen,last_seen=excluded.last_seen,data=excluded.data")
	if err != nil {
		return err
	}
	defer connectionStmt.Close()
	for id, next := range changed {
		data, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if _, err = connectionStmt.ExecContext(ctx, id, next.Connection.FirstSeen.Unix(), next.Connection.LastSeen.Unix(), string(data)); err != nil {
			return err
		}
	}
	deltaStmt, err := tx.PrepareContext(ctx, `INSERT INTO traffic_samples(at,connection,session,upload,download,gap_upload,gap_download,samples) VALUES(?,?,?,?,?,?,?,1)
ON CONFLICT(at,connection,session) DO UPDATE SET upload=upload+excluded.upload,download=download+excluded.download,gap_upload=gap_upload+excluded.gap_upload,gap_download=gap_download+excluded.gap_download,samples=samples+1`)
	if err != nil {
		return err
	}
	defer deltaStmt.Close()
	for _, delta := range deltas {
		gapUp, gapDown := int64(0), int64(0)
		if delta.Gap {
			gapUp, gapDown = delta.Upload, delta.Download
		}
		if _, err = deltaStmt.ExecContext(ctx, delta.At, delta.Connection, delta.Session, delta.Upload, delta.Download, gapUp, gapDown); err != nil {
			return err
		}
	}
	if len(deltas) > 0 {
		for _, candidate := range candidates {
			data, err := json.Marshal(candidate.Session)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO traffic_sessions(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", candidate.Session.ID, string(data)); err != nil {
				return err
			}
		}
	}
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO traffic_meta(key,data) VALUES('status',?) ON CONFLICT(key) DO UPDATE SET data=excluded.data", string(data)); err != nil {
		return err
	}
	return tx.Commit()
}

// Read all aggregates from one snapshot, so a live collector commit cannot
// make totals disagree with the session/unallocated or connection breakdown.
func (s *liveTrafficStore) report(ctx context.Context, report *liveTrafficReport) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	from, to := report.From.Unix(), report.To.Unix()
	rows, err := tx.QueryContext(ctx, `SELECT connection,session,SUM(upload),SUM(download),SUM(gap_upload),SUM(gap_download),SUM(samples) FROM traffic_samples WHERE at>=? AND at<? GROUP BY connection,session`, from, to)
	if err != nil {
		return err
	}
	connections := map[string]liveTrafficBytes{}
	connectionSamples := map[string]int64{}
	sessions := map[string]liveTrafficSession{}
	for rows.Next() {
		var connection, session string
		var upload, download, gapUp, gapDown, samples int64
		if err = rows.Scan(&connection, &session, &upload, &download, &gapUp, &gapDown, &samples); err != nil {
			break
		}
		report.Totals.Upload += upload
		report.Totals.Download += download
		report.Gap.Upload += gapUp
		report.Gap.Download += gapDown
		b := connections[connection]
		b.Upload += upload
		b.Download += download
		connections[connection] = b
		connectionSamples[connection] += samples
		if session == "" {
			report.Unallocated.Upload += upload
			report.Unallocated.Download += download
		} else {
			report.Allocated.Upload += upload
			report.Allocated.Download += download
			value := sessions[session]
			value.ID = session
			value.Upload += upload
			value.Download += download
			value.Samples += samples
			sessions[session] = value
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	// Include zero-byte active connections for observability, while completed
	// historical connections appear only when they have usage in this range.
	rows, err = tx.QueryContext(ctx, `SELECT id,data FROM traffic_connections WHERE last_seen>=? AND first_seen<?`, from, to)
	if err != nil {
		return err
	}
	processes := map[string]liveTrafficProcess{}
	for rows.Next() {
		var id string
		var data []byte
		var checkpoint liveTrafficCheckpoint
		if err = rows.Scan(&id, &data); err != nil {
			break
		}
		if err = json.Unmarshal(data, &checkpoint); err != nil {
			break
		}
		connection := checkpoint.Connection
		connection.Category = "other"
		if liveTrafficModelCandidate(connection.Process, connection.Host, connection.Network) {
			connection.Category = "model_candidate"
		}
		b, used := connections[id]
		if !used && !connection.Active {
			continue
		}
		connection.Upload, connection.Download, connection.Samples = b.Upload, b.Download, connectionSamples[id]
		report.Connections = append(report.Connections, connection)
		key := connection.Process + ":" + strconv.FormatUint(uint64(connection.PID), 10)
		process := processes[key]
		process.Process, process.PID = connection.Process, connection.PID
		process.Upload += b.Upload
		process.Download += b.Download
		process.Connections++
		processes[key] = process
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, process := range processes {
		report.Processes = append(report.Processes, process)
	}
	for id, session := range sessions {
		var data []byte
		if err := tx.QueryRowContext(ctx, "SELECT data FROM traffic_sessions WHERE id=?", id).Scan(&data); err == nil {
			var metadata liveTrafficSession
			if err := json.Unmarshal(data, &metadata); err != nil {
				return err
			}
			session.Title, session.Model, session.ParentID = metadata.Title, metadata.Model, metadata.ParentID
		} else if err != sql.ErrNoRows {
			return err
		}
		if session.Title == "" {
			session.Title = "未命名对话"
		}
		session.Estimated = true
		report.Sessions = append(report.Sessions, session)
	}
	// Preserve saved ancestry even when a parent's log is no longer loaded.
	// These zero-usage rows are context, not additional traffic estimates.
	knownSessions := make(map[string]bool, len(report.Sessions))
	for _, session := range report.Sessions {
		knownSessions[session.ID] = true
	}
	for i := 0; i < len(report.Sessions); i++ {
		parent := report.Sessions[i].ParentID
		if parent == "" || knownSessions[parent] {
			continue
		}
		knownSessions[parent] = true
		var data []byte
		metadata := liveTrafficSession{ID: parent}
		if err := tx.QueryRowContext(ctx, "SELECT data FROM traffic_sessions WHERE id=?", parent).Scan(&data); err == nil {
			if err := json.Unmarshal(data, &metadata); err != nil {
				return err
			}
		} else if err != sql.ErrNoRows {
			return err
		}
		metadata.ID, metadata.ContextOnly, metadata.Estimated = parent, true, true
		metadata.Upload, metadata.Download, metadata.Samples = 0, 0, 0
		report.Sessions = append(report.Sessions, metadata)
	}
	// At most about 1,500 plot points, even for a month. Totals retain the full
	// minute data; only the plot's presentation buckets become wider.
	bucket := max(int64(60), ((to-from+1499)/1500+59)/60*60)
	rows, err = tx.QueryContext(ctx, `SELECT (at/?)*?,SUM(upload),SUM(download) FROM traffic_samples WHERE at>=? AND at<? GROUP BY (at/?)*? ORDER BY 1`, bucket, bucket, from, to, bucket, bucket)
	if err != nil {
		return err
	}
	for rows.Next() {
		var at, upload, download int64
		if err = rows.Scan(&at, &upload, &download); err != nil {
			break
		}
		pointAt := max(at, from)
		report.Points = append(report.Points, liveTrafficPoint{At: time.Unix(pointAt, 0).UTC(), Upload: upload, Download: download, Seconds: int(min(at+bucket, to) - pointAt)})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	sort.Slice(report.Connections, func(i, j int) bool {
		return report.Connections[i].Upload+report.Connections[i].Download > report.Connections[j].Upload+report.Connections[j].Download
	})
	sort.Slice(report.Processes, func(i, j int) bool {
		return report.Processes[i].Upload+report.Processes[i].Download > report.Processes[j].Upload+report.Processes[j].Download
	})
	sort.Slice(report.Sessions, func(i, j int) bool {
		return report.Sessions[i].Upload+report.Sessions[i].Download > report.Sessions[j].Upload+report.Sessions[j].Download
	})
	report.ConnectionCount = len(report.Connections)
	if len(report.Connections) > 200 {
		report.Connections = report.Connections[:200]
		report.ConnectionsTruncated = true
	}
	return tx.Commit()
}
