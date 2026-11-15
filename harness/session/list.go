package sessionformat

// SessionListPage contains canonical headers found in a user's file-backed
// Host. A reservation without a v4 journal is not a listed Session.
type SessionListPage struct {
	Sessions   []Header `json:"sessions"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

const maxV4HeaderLineBytes = 64 << 10
