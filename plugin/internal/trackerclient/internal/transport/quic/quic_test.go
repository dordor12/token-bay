package quic

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
)

func TestDialReturnsErrorOnMissingServer(t *testing.T) {
	d := New()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := d.Dial(ctx, transport.Endpoint{Addr: "127.0.0.1:1"}, nil)
	assert.Error(t, err)
}
