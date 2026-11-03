package models

import "database/sql"

type Artist struct {
	ID   string
	Name string
	MBID sql.NullString
}

type Album struct {
	ID       string
	Title    string
	Artist   string
	ArtistID string
	Year     int64
	CoverArt sql.NullString
	MBID     sql.NullString
}

type Song struct {
	ID          string
	Path        string
	Title       string
	Artist      string
	ArtistID    string
	Album       string
	AlbumID     string
	Genre       sql.NullString
	TrackNumber sql.NullInt64
	DiscNumber  sql.NullInt64
	Year        sql.NullInt64
	DurationMs  int64
	Bitrate     int64
	Filesize    int64
	Suffix      string
	ContentType string
	CoverArt    sql.NullString
	MBID        sql.NullString
	Tags        sql.NullString // JSON object of allowlisted file tags
}

type Playlist struct {
	ID        string
	Name      string
	Comment   sql.NullString
	Public    int64
	CreatedAt string
	UpdatedAt string
}
