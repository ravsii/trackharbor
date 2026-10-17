package store

import (
	"bytes"
	"database/sql"
	"encoding/gob"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ravsii/trackharbor/download"

	_ "github.com/mattn/go-sqlite3"
)

type Track struct {
	ID                              int64
	Source, SourceID, Artist, Title string
	AddedAt                         int64
	AudioURL, CoverURL, FilePath    string
	DownloadAttempts                int
	DownloadFailed                  bool
}
type Store struct {
	db          *sql.DB
	downloadDir string
}

func Open(path string, directories ...string) (*Store, error) {
	downloadDir := filepath.Join(filepath.Dir(path), "downloads")
	if len(directories) != 0 && directories[0] != "" {
		downloadDir = directories[0]
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("_foreign_keys", "on")
	q.Set("_busy_timeout", "5000")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate server database: %w", err)
	}
	s := &Store{db: db, downloadDir: downloadDir}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func validSource(source string) bool { return source == "spotify" || source == "vk" }
func (s *Store) SaveCredentials(userID int64, source string, credentials any) error {
	var data bytes.Buffer
	if err := gob.NewEncoder(&data).Encode(credentials); err != nil {
		return fmt.Errorf("encode source credentials: %w", err)
	}
	_, err := s.db.Exec(`INSERT INTO user_credentials(user_id,source,data) VALUES(?,?,?) ON CONFLICT(user_id,source) DO UPDATE SET data=excluded.data`, userID, source, data.Bytes())
	return err
}
func (s *Store) Credentials(userID int64, source string, credentials any) error {
	var data []byte
	if err := s.db.QueryRow(`SELECT data FROM user_credentials WHERE user_id=? AND source=?`, userID, source).Scan(&data); err != nil {
		return err
	}
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(credentials); err != nil {
		return fmt.Errorf("decode source credentials; reconnect source: %w", err)
	}
	return nil
}
func (s *Store) Connected(userID int64, source string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM user_credentials WHERE user_id=? AND source=?`, userID, source).Scan(&n)
	return n > 0, err
}

const catalogSelect = `SELECT t.id,t.source,t.source_key,t.artist,t.name,t.cover_url,0,t.source_url,t.download_attempts,t.download_failed FROM tracks t`
const personalSelect = `SELECT t.id,t.source,t.source_key,t.artist,t.name,t.cover_url,p.added_at,t.source_url,t.download_attempts,t.download_failed
 FROM user_tracks p JOIN tracks t ON t.id=p.track_id`

func (s *Store) List(userID int64, source string) ([]Track, error) {
	return s.queryTracks(personalSelect+` WHERE p.user_id=? AND t.source=? ORDER BY p.added_at DESC,t.artist,t.name,t.id`, userID, source)
}
func (s *Store) ListUser(userID int64) ([]Track, error) {
	return s.queryTracks(personalSelect+` WHERE p.user_id=? ORDER BY p.added_at DESC,t.source,t.artist,t.name,t.id`, userID)
}
func (s *Store) ListAll() ([]Track, error) {
	return s.queryTracks(catalogSelect + ` ORDER BY t.id DESC`)
}

// Downloadable excludes orphaned catalog entries; their completed files remain available.
func (s *Store) Downloadable() ([]Track, error) {
	return s.queryTracks(catalogSelect + ` WHERE EXISTS(SELECT 1 FROM user_tracks p WHERE p.track_id=t.id) ORDER BY t.id DESC`)
}
func (s *Store) ListPlayable(userID int64) ([]Track, error) {
	tracks, err := s.ListUser(userID)
	if err != nil {
		return nil, err
	}
	items := []Track{}
	for _, t := range tracks {
		info, err := os.Stat(t.FilePath)
		if err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			items = append(items, t)
		}
	}
	return items, nil
}
func (s *Store) Track(id int64) (Track, error) {
	items, err := s.queryTracks(catalogSelect+` WHERE t.id=?`, id)
	if err != nil {
		return Track{}, err
	}
	if len(items) == 0 {
		return Track{}, sql.ErrNoRows
	}
	return items[0], nil
}
func (s *Store) queryTracks(query string, args ...any) ([]Track, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Track{}
	for rows.Next() {
		var t Track
		if err = rows.Scan(&t.ID, &t.Source, &t.SourceID, &t.Artist, &t.Title, &t.CoverURL, &t.AddedAt, &t.AudioURL, &t.DownloadAttempts, &t.DownloadFailed); err != nil {
			return nil, err
		}
		path, err := download.FilePath(s.downloadDir, t.Source, t.SourceID)
		if err != nil {
			return nil, err
		}
		if download.FileExists(path) {
			t.FilePath = path
		}
		items = append(items, t)
	}
	return items, rows.Err()
}
func (s *Store) SetSourceURL(id int64, sourceURL string) error {
	_, err := s.db.Exec(`UPDATE tracks SET source_url=? WHERE id=?`, sourceURL, id)
	return err
}

func (s *Store) RecordDownloadFailure(id int64, limit int) error {
	_, err := s.db.Exec(`UPDATE tracks SET download_attempts=download_attempts+1,
		download_failed=CASE WHEN download_attempts+1>=? THEN 1 ELSE download_failed END WHERE id=?`, limit, id)
	return err
}

func (s *Store) SetDownloadFailed(id int64) error {
	_, err := s.db.Exec(`UPDATE tracks SET download_failed=1 WHERE id=?`, id)
	return err
}

// Replace changes only this user's source membership. Catalog rows and files survive.
func (s *Store) Replace(userID int64, source string, tracks []Track) (int, error) {
	if !validSource(source) {
		return 0, fmt.Errorf("unknown source")
	}
	for _, t := range tracks {
		if strings.TrimSpace(t.SourceID) == "" || len(t.SourceID) > 512 {
			return 0, fmt.Errorf("source track ID is missing or invalid")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRow(`SELECT id FROM users WHERE id=?`, userID).Scan(&exists); err != nil {
		return 0, err
	}
	previous := map[int64]int64{}
	rows, err := tx.Query(`SELECT p.track_id,p.added_at FROM user_tracks p JOIN tracks t ON t.id=p.track_id WHERE p.user_id=? AND t.source=?`, userID, source)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id, added int64
		if err = rows.Scan(&id, &added); err != nil {
			rows.Close()
			return 0, err
		}
		previous[id] = added
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`DELETE FROM user_tracks WHERE user_id=? AND track_id IN (SELECT id FROM tracks WHERE source=?)`, userID, source); err != nil {
		return 0, err
	}
	count := 0
	for _, t := range tracks {
		if _, err = tx.Exec(`INSERT INTO tracks(source,source_key,artist,name,cover_url,source_url) VALUES(?,?,?,?,?,?)
 ON CONFLICT(source,source_key) DO UPDATE SET artist=excluded.artist,name=excluded.name,cover_url=excluded.cover_url,
 source_url=CASE WHEN excluded.source_url<>'' THEN excluded.source_url ELSE tracks.source_url END`, source, t.SourceID, t.Artist, t.Title, t.CoverURL, t.AudioURL); err != nil {
			return 0, err
		}
		var id int64
		if err = tx.QueryRow(`SELECT id FROM tracks WHERE source=? AND source_key=?`, source, t.SourceID).Scan(&id); err != nil {
			return 0, err
		}
		added := t.AddedAt
		if added == 0 {
			added = previous[id]
		}
		if added == 0 && source == "vk" {
			added = time.Now().Unix()
		}
		result, err := tx.Exec(`INSERT OR IGNORE INTO user_tracks(user_id,track_id,added_at) VALUES(?,?,?)`, userID, id, added)
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		count += int(n)
	}
	return count, tx.Commit()
}
