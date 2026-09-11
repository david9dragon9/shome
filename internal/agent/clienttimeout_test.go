package agent

import "testing"

// A heartbeat should time out; a stream should not.
//
// They shared one client with a ten-second timeout, so an interactive session
// stopped accepting input after ten seconds and a file transfer bigger than
// ten seconds of bandwidth could never finish. Neither failed loudly: a
// timeout on a connection that is idle by design looks exactly like the far
// end having nothing to say, so `srun --pty` simply stopped responding and a
// transfer simply stalled.
func TestStreamsHaveNoTimeoutButHeartbeatsDo(t *testing.T) {
	c, err := NewClient("127.0.0.1:1", "n", nil, nil, nil, "localhost")
	if err != nil {
		// Without certificates NewClient may refuse; the fields are what
		// matter and they are set together.
		t.Skipf("client needs credentials here: %v", err)
	}
	if c.http.Timeout == 0 {
		t.Error("the heartbeat client has no timeout; a dropped request would " +
			"hang the agent instead of being retried")
	}
	if c.long.Timeout != 0 {
		t.Errorf("the streaming client has a %v timeout; an interactive session "+
			"or a large transfer would be cut off mid-way", c.long.Timeout)
	}
	if c.http.Transport != c.long.Transport {
		t.Error("the two clients should share a transport, so connection " +
			"pooling and TLS setup are not duplicated")
	}
}
