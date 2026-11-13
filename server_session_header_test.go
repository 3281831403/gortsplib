package gortsplib

import (
	"bufio"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/conn"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/headers"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
)

// startRawSessionTestServer starts an in-process server with a single H264
// stream on an ephemeral TCP port and returns the server, its stream and address.
func startRawSessionTestServer(t *testing.T) (*Server, *ServerStream, string) {
	t.Helper()

	var address string
	var stream *ServerStream

	s := &Server{
		Handler: &testServerHandler{
			onDescribe: func(_ *ServerHandlerOnDescribeCtx) (*base.Response, *ServerStream, error) {
				return &base.Response{
					StatusCode: base.StatusOK,
				}, stream, nil
			},
			onSetup: func(_ *ServerHandlerOnSetupCtx) (*base.Response, *ServerStream, error) {
				return &base.Response{
					StatusCode: base.StatusOK,
				}, stream, nil
			},
			onPlay: func(_ *ServerHandlerOnPlayCtx) (*base.Response, error) {
				return &base.Response{
					StatusCode: base.StatusOK,
				}, nil
			},
			onPause: func(_ *ServerHandlerOnPauseCtx) (*base.Response, error) {
				return &base.Response{
					StatusCode: base.StatusOK,
				}, nil
			},
		},
		RTSPAddress: "127.0.0.1:0",
		Listen: func(network, address2 string) (net.Listener, error) {
			ln, err := net.Listen(network, address2)
			if err == nil {
				address = ln.Addr().String()
			}
			return ln, err
		},
	}
	require.NoError(t, s.Start())
	t.Cleanup(s.Close)

	stream = &ServerStream{
		Server: s,
		Desc:   &description.Session{Medias: []*description.Media{testH264Media}},
	}
	require.NoError(t, stream.Initialize())
	t.Cleanup(stream.Close)

	return s, stream, address
}

func newRawTestConn(t *testing.T, address string) (net.Conn, *conn.Conn) {
	t.Helper()

	nconn, err := net.Dial("tcp", address)
	require.NoError(t, err)
	t.Cleanup(func() { nconn.Close() })

	return nconn, conn.NewConn(bufio.NewReader(nconn), nconn)
}

// writeRaw writes a raw RTSP message, allowing header values that cannot be
// expressed through the base.Request marshaler.
func writeRaw(t *testing.T, nconn net.Conn, raw string) {
	t.Helper()
	_, err := nconn.Write([]byte(raw))
	require.NoError(t, err)
}

// describeSetup performs DESCRIBE + SETUP over the connection and returns the
// session id contained in the SETUP response.
func describeSetup(t *testing.T, c *conn.Conn, address string) string {
	t.Helper()

	desc := doDescribe(t, c, false)

	inTH := &headers.Transport{
		Protocol:       headers.TransportProtocolTCP,
		Delivery:       new(headers.TransportDeliveryUnicast),
		Mode:           new(headers.TransportModePlay),
		InterleavedIDs: &[2]int{0, 1},
	}

	res, _ := doSetup(t, c, mediaURL(t, desc.BaseURL, desc.Medias[0]).String(), inTH, "")

	return readSession(t, res)
}

// TestServerSessionHeaderParameters verifies that the server identifies
// sessions by the bare session id and tolerates the ";timeout=" parameter and
// leading or trailing spaces that some cameras append when echoing Session.
func TestServerSessionHeaderParameters(t *testing.T) {
	for _, ca := range []string{
		"timeout parameter",
		"timeout parameter with space",
		"leading space",
		"trailing space",
		"surrounding spaces and timeout",
	} {
		t.Run(ca, func(t *testing.T) {
			_, _, address := startRawSessionTestServer(t)
			nconn, c := newRawTestConn(t, address)

			sessionID := describeSetup(t, c, address)

			var sessionHeader string
			switch ca {
			case "timeout parameter":
				sessionHeader = sessionID + ";timeout=55"

			case "timeout parameter with space":
				sessionHeader = sessionID + "; timeout=55"

			case "leading space":
				sessionHeader = " " + sessionID

			case "trailing space":
				sessionHeader = sessionID + " "

			case "surrounding spaces and timeout":
				sessionHeader = " " + sessionID + " ;timeout=55 "
			}

			// PLAY must succeed even though the session id is decorated.
			writeRaw(t, nconn,
				"PLAY rtsp://"+address+"/teststream RTSP/1.0\r\n"+
					"CSeq: 2\r\n"+
					"Session: "+sessionHeader+"\r\n"+
					"\r\n")
			res := readResponseIgnoreFramesMust(t, c)
			require.Equal(t, base.StatusOK, res.StatusCode)

			// PAUSE must succeed as well.
			writeRaw(t, nconn,
				"PAUSE rtsp://"+address+"/teststream RTSP/1.0\r\n"+
					"CSeq: 3\r\n"+
					"Session: "+sessionHeader+"\r\n"+
					"\r\n")
			res = readResponseIgnoreFramesMust(t, c)
			require.Equal(t, base.StatusOK, res.StatusCode)

			// the connection must still accept session-less requests.
			res, err := writeReqReadRes(c, base.Request{
				Method: base.Options,
				URL:    mustParseURL("rtsp://" + address + "/"),
				Header: base.Header{
					"CSeq": base.HeaderValue{"4"},
				},
			})
			require.NoError(t, err)
			require.Equal(t, base.StatusOK, res.StatusCode)
		})
	}
}

func readResponseIgnoreFramesMust(t *testing.T, c *conn.Conn) *base.Response {
	t.Helper()
	res, err := readResponseIgnoreFrames(c)
	require.NoError(t, err)
	return res
}

// TestServerSessionRejectionKeepsConnectionAlive verifies that unknown session
// ids are rejected with 454 and syntactically invalid Session headers with 400,
// and that both rejections leave the TCP connection usable, including for a new
// DESCRIBE + SETUP + PLAY sequence on the same connection.
func TestServerSessionRejectionKeepsConnectionAlive(t *testing.T) {
	t.Run("unknown session id", func(t *testing.T) {
		_, _, address := startRawSessionTestServer(t)
		nconn, c := newRawTestConn(t, address)

		// a PLAY referencing an unknown session must result in 454.
		writeRaw(t, nconn,
			"PLAY rtsp://"+address+"/teststream RTSP/1.0\r\n"+
				"CSeq: 1\r\n"+
				"Session: unknownsession1234\r\n"+
				"\r\n")
		res := readResponseIgnoreFramesMust(t, c)
		require.Equal(t, base.StatusSessionNotFound, res.StatusCode)

		// the connection is still alive: OPTIONS works.
		res, err := writeReqReadRes(c, base.Request{
			Method: base.Options,
			URL:    mustParseURL("rtsp://" + address + "/"),
			Header: base.Header{
				"CSeq": base.HeaderValue{"2"},
			},
		})
		require.NoError(t, err)
		require.Equal(t, base.StatusOK, res.StatusCode)

		// a brand new session can be created on the same connection.
		sessionID := describeSetup(t, c, address)

		res, err = writeReqReadRes(c, base.Request{
			Method: base.Play,
			URL:    mustParseURL("rtsp://" + address + "/teststream"),
			Header: base.Header{
				"CSeq":    base.HeaderValue{"4"},
				"Session": base.HeaderValue{sessionID},
			},
		})
		require.NoError(t, err)
		require.Equal(t, base.StatusOK, res.StatusCode)
	})

	t.Run("invalid session header syntax", func(t *testing.T) {
		_, _, address := startRawSessionTestServer(t)
		nconn, c := newRawTestConn(t, address)

		// an empty session id (only the parameter section) is invalid syntax.
		writeRaw(t, nconn,
			"PLAY rtsp://"+address+"/teststream RTSP/1.0\r\n"+
				"CSeq: 1\r\n"+
				"Session: ;timeout=55\r\n"+
				"\r\n")
		res := readResponseIgnoreFramesMust(t, c)
		require.Equal(t, base.StatusBadRequest, res.StatusCode)

		// a non-numeric timeout also results in 400.
		writeRaw(t, nconn,
			"PLAY rtsp://"+address+"/teststream RTSP/1.0\r\n"+
				"CSeq: 2\r\n"+
				"Session: 1234;timeout=abc\r\n"+
				"\r\n")
		res = readResponseIgnoreFramesMust(t, c)
		require.Equal(t, base.StatusBadRequest, res.StatusCode)

		// a whitespace-only session id is invalid syntax as well.
		writeRaw(t, nconn,
			"PLAY rtsp://"+address+"/teststream RTSP/1.0\r\n"+
				"CSeq: 3\r\n"+
				"Session:    \r\n"+
				"\r\n")
		res = readResponseIgnoreFramesMust(t, c)
		require.Equal(t, base.StatusBadRequest, res.StatusCode)

		// multiple Session headers are rejected as invalid syntax.
		writeRaw(t, nconn,
			"PLAY rtsp://"+address+"/teststream RTSP/1.0\r\n"+
				"CSeq: 4\r\n"+
				"Session: abc\r\n"+
				"Session: def\r\n"+
				"\r\n")
		res = readResponseIgnoreFramesMust(t, c)
		require.Equal(t, base.StatusBadRequest, res.StatusCode)

		// the connection is still alive: DESCRIBE works.
		res, err := writeReqReadRes(c, base.Request{
			Method: base.Describe,
			URL:    mustParseURL("rtsp://" + address + "/teststream"),
			Header: base.Header{
				"CSeq": base.HeaderValue{"5"},
			},
		})
		require.NoError(t, err)
		require.Equal(t, base.StatusOK, res.StatusCode)

		// and a new session can be created and played on the same connection.
		sessionID := describeSetup(t, c, address)

		writeRaw(t, nconn,
			"PLAY rtsp://"+address+"/teststream RTSP/1.0\r\n"+
				"CSeq: 7\r\n"+
				"Session: "+sessionID+";timeout=55\r\n"+
				"\r\n")
		res = readResponseIgnoreFramesMust(t, c)
		require.Equal(t, base.StatusOK, res.StatusCode)
	})

	t.Run("torn down id then new session on same connection", func(t *testing.T) {
		_, _, address := startRawSessionTestServer(t)
		nconn, c := newRawTestConn(t, address)

		// create and tear down a first session.
		firstID := describeSetup(t, c, address)

		res, err := writeReqReadRes(c, base.Request{
			Method: base.Teardown,
			URL:    mustParseURL("rtsp://" + address + "/teststream"),
			Header: base.Header{
				"CSeq":    base.HeaderValue{"2"},
				"Session": base.HeaderValue{firstID},
			},
		})
		require.NoError(t, err)
		require.Equal(t, base.StatusOK, res.StatusCode)

		// the torn-down id is now unknown: 454, without disconnecting.
		writeRaw(t, nconn,
			"PLAY rtsp://"+address+"/teststream RTSP/1.0\r\n"+
				"CSeq: 3\r\n"+
				"Session: "+firstID+";timeout=55\r\n"+
				"\r\n")
		res = readResponseIgnoreFramesMust(t, c)
		require.Equal(t, base.StatusSessionNotFound, res.StatusCode)

		// a second session can be established on the same connection.
		secondID := describeSetup(t, c, address)
		require.NotEqual(t, firstID, secondID)

		res, err = writeReqReadRes(c, base.Request{
			Method: base.Play,
			URL:    mustParseURL("rtsp://" + address + "/teststream"),
			Header: base.Header{
				"CSeq":    base.HeaderValue{"5"},
				"Session": base.HeaderValue{secondID + " "},
			},
		})
		require.NoError(t, err)
		require.Equal(t, base.StatusOK, res.StatusCode)
	})
}

// TestServerSessionFatalErrorsStillClose verifies that the errors that were
// historically fatal still cause the TCP connection to be closed.
func TestServerSessionFatalErrorsStillClose(t *testing.T) {
	t.Run("missing CSeq", func(t *testing.T) {
		connClosed := make(chan struct{})

		_, _, address := startRawSessionTestServerWithClose(t, func(_ *ServerHandlerOnConnCloseCtx) {
			close(connClosed)
		})

		nconn, _ := newRawTestConn(t, address)

		writeRaw(t, nconn,
			"OPTIONS rtsp://"+address+"/ RTSP/1.0\r\n"+
				"\r\n")

		select {
		case <-connClosed:
		case <-time.After(2 * time.Second):
			t.Fatal("connection was not closed after missing CSeq")
		}
	})

	t.Run("invalid URL", func(t *testing.T) {
		connClosed := make(chan struct{})

		_, _, address := startRawSessionTestServerWithClose(t, func(ctx *ServerHandlerOnConnCloseCtx) {
			require.IsType(t, liberrors.ErrServerInvalidPath{}, ctx.Error)
			close(connClosed)
		})

		nconn, _ := newRawTestConn(t, address)

		// an OPTIONS request is allowed with a nil URL ("*"), while every
		// other method rejects it with ErrServerInvalidPath and disconnects.
		writeRaw(t, nconn,
			"DESCRIBE * RTSP/1.0\r\n"+
				"CSeq: 1\r\n"+
				"\r\n")

		select {
		case <-connClosed:
		case <-time.After(2 * time.Second):
			t.Fatal("connection was not closed after invalid URL")
		}
	})

	t.Run("authentication failure", func(t *testing.T) {
		connClosed := make(chan struct{})
		var address string

		s := &Server{
			Handler: &testServerHandler{
				onConnClose: func(ctx *ServerHandlerOnConnCloseCtx) {
					require.EqualError(t, ctx.Error, "authentication error")
					close(connClosed)
				},
				onAnnounce: func(ctx *ServerHandlerOnAnnounceCtx) (*base.Response, error) {
					ok := ctx.Conn.VerifyCredentials(ctx.Request, "myuser", "mypass")
					if !ok {
						return &base.Response{
							StatusCode: base.StatusUnauthorized,
						}, liberrors.ErrServerAuth{}
					}
					return &base.Response{
						StatusCode: base.StatusOK,
					}, nil
				},
			},
			RTSPAddress: "127.0.0.1:0",
			Listen: func(network, address2 string) (net.Listener, error) {
				ln, err := net.Listen(network, address2)
				if err == nil {
					address = ln.Addr().String()
				}
				return ln, err
			},
		}
		require.NoError(t, s.Start())
		t.Cleanup(s.Close)

		nconn, c := newRawTestConn(t, address)

		sdp := mediasToSDP([]*description.Media{testH264Media})

		// first request without credentials gets a 401 and stays open.
		writeRaw(t, nconn,
			"ANNOUNCE rtsp://"+address+"/teststream RTSP/1.0\r\n"+
				"CSeq: 1\r\n"+
				"Content-Type: application/sdp\r\n"+
				"Content-Length: "+strconv.FormatInt(int64(len(sdp)), 10)+"\r\n"+
				"\r\n"+
				string(sdp))
		res := readResponseIgnoreFramesMust(t, c)
		require.Equal(t, base.StatusUnauthorized, res.StatusCode)

		// second request with bogus credentials closes the connection.
		writeRaw(t, nconn,
			"ANNOUNCE rtsp://"+address+"/teststream RTSP/1.0\r\n"+
				"CSeq: 2\r\n"+
				"Content-Type: application/sdp\r\n"+
				"Content-Length: "+strconv.FormatInt(int64(len(sdp)), 10)+"\r\n"+
				"Authorization: Basic d3Jvbmd1c2VyOndyb25ncGFzcw==\r\n"+
				"\r\n"+
				string(sdp))
		res = readResponseIgnoreFramesMust(t, c)
		require.Equal(t, base.StatusUnauthorized, res.StatusCode)

		select {
		case <-connClosed:
		case <-time.After(2 * time.Second):
			t.Fatal("connection was not closed after authentication failure")
		}
	})
}

// startRawSessionTestServerWithClose is startRawSessionTestServer with an
// additional OnConnClose callback.
func startRawSessionTestServerWithClose(
	t *testing.T,
	onClose func(*ServerHandlerOnConnCloseCtx),
) (*Server, *ServerStream, string) {
	t.Helper()

	var address string
	var stream *ServerStream

	s := &Server{
		Handler: &testServerHandler{
			onConnClose: onClose,
			onDescribe: func(_ *ServerHandlerOnDescribeCtx) (*base.Response, *ServerStream, error) {
				return &base.Response{
					StatusCode: base.StatusOK,
				}, stream, nil
			},
		},
		RTSPAddress: "127.0.0.1:0",
		Listen: func(network, address2 string) (net.Listener, error) {
			ln, err := net.Listen(network, address2)
			if err == nil {
				address = ln.Addr().String()
			}
			return ln, err
		},
	}
	require.NoError(t, s.Start())
	t.Cleanup(s.Close)

	stream = &ServerStream{
		Server: s,
		Desc:   &description.Session{Medias: []*description.Media{testH264Media}},
	}
	require.NoError(t, stream.Initialize())
	t.Cleanup(stream.Close)

	return s, stream, address
}
