package sessionformat

import "context"

func v4Claim(context.Context, string) (func(), error) { return func() {}, nil }

func v4TestHeader(id string) Header {
	return Header{Type: "session", Version: Version, ID: id, CreatedAt: 1_700_000_000_000}
}
