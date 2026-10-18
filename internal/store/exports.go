package store

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/ravsii/trackharbor/download"
)

type ExportTrack struct {
	Name string
	Path string // Relative to the download directory; never sent to clients.
}

// ExportTracks assigns readable names once, so a later sync or name collision
// cannot make a client download an existing track under a different filename.
func (s *Store) ExportTracks(ctx context.Context, userID int64) ([]ExportTrack, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT t.id,t.source,t.source_key,t.artist,t.name,COALESCE(e.name,'')
		FROM user_tracks p JOIN tracks t ON t.id=p.track_id
		LEFT JOIN track_export_names e ON e.track_id=t.id WHERE p.user_id=? ORDER BY t.id`, userID)
	if err != nil {
		return nil, err
	}
	type entry struct {
		id                               int64
		source, key, artist, title, name string
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.source, &e.key, &e.artist, &e.title, &e.name); err != nil {
			rows.Close()
			return nil, err
		}
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	tracks := make([]ExportTrack, 0, len(entries))
	for _, e := range entries {
		if e.name == "" {
			base := exportNamePart(e.artist, "Unknown artist") + " - " + exportNamePart(e.title, "Untitled")
			for suffix := 0; ; suffix++ {
				name := base + ".mp3"
				if suffix == 1 {
					name = fmt.Sprintf("%s [%d].mp3", base, e.id)
				} else if suffix > 1 {
					name = fmt.Sprintf("%s [%d-%d].mp3", base, e.id, suffix)
				}
				result, err := tx.ExecContext(ctx, `INSERT INTO track_export_names(track_id,name,name_key) VALUES(?,?,?)
					ON CONFLICT(name_key) DO NOTHING`, e.id, name, cases.Fold().String(name))
				if err != nil {
					return nil, err
				}
				n, err := result.RowsAffected()
				if err != nil {
					return nil, err
				}
				if n != 0 {
					e.name = name
					break
				}
			}
		}
		path, err := download.FilePath("", e.source, e.key)
		if err != nil {
			return nil, err
		}
		tracks = append(tracks, ExportTrack{Name: e.name, Path: path})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return tracks, nil
}

// ExportTrack checks current membership on every download, including requests
// for a previously listed name after a track has been removed from the library.
func (s *Store) ExportTrack(ctx context.Context, userID int64, name string) (ExportTrack, error) {
	var source, key string
	err := s.db.QueryRowContext(ctx, `SELECT t.source,t.source_key FROM track_export_names e
		JOIN tracks t ON t.id=e.track_id JOIN user_tracks p ON p.track_id=t.id
		WHERE p.user_id=? AND e.name=?`, userID, name).Scan(&source, &key)
	if err != nil {
		return ExportTrack{}, err
	}
	path, err := download.FilePath("", source, key)
	return ExportTrack{Name: name, Path: path}, err
}

func exportNamePart(value, fallback string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, norm.NFC.String(value))
	// Leave room for the other name part, extension and collision suffix on
	// filesystems with a 255-byte filename limit. Never split a UTF-8 character.
	for i := range value {
		if i >= 90 {
			value = value[:i]
			break
		}
	}
	value = strings.Trim(strings.TrimSpace(value), ". ")
	if value == "" {
		return fallback
	}
	return value
}
