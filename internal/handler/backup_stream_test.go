package handler

import (
	"errors"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type testBackupStream struct {
	chunks []*pb.HostBackupChunk
	err    error
}

func (s *testBackupStream) Recv() (*pb.HostBackupChunk, error) {
	if len(s.chunks) == 0 {
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}
func TestBackupStreamBinaryAndBounded(t *testing.T) {
	w := httptest.NewRecorder()
	s := &testBackupStream{chunks: []*pb.HostBackupChunk{{Filename: "../../evil\r\nInjected.txt", Data: []byte("first")}, {Data: []byte("second")}}}
	if e := streamBackupResponse(w, s); e != nil {
		t.Fatal(e)
	}
	if w.Body.String() != "firstsecond" || w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatal("not streamed")
	}
	if e := streamBackupResponse(httptest.NewRecorder(), &testBackupStream{chunks: []*pb.HostBackupChunk{{Data: make([]byte, 65537)}}}); e == nil {
		t.Fatal("oversized chunk accepted")
	}
}
func TestBackupStreamLateFailureAborts(t *testing.T) {
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Fatal("late stream failure did not abort")
		}
	}()
	streamBackupResponse(httptest.NewRecorder(), &testBackupStream{chunks: []*pb.HostBackupChunk{{Filename: "b.tar", Data: []byte("prefix")}}, err: errors.New("lost stream")})
}
