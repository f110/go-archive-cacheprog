package cacheprog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

func Run(ctx context.Context, archivePath string, method Compression, in io.Reader, out, stats io.Writer) (err error) {
	cache, err := OpenArchive(archivePath, method)
	if err != nil {
		return fmt.Errorf("failed to open archive %q: %w", archivePath, err)
	}
	defer func() {
		if stats != nil {
			_ = cache.WriteStats(stats)
		}
		if cerr := cache.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	return Serve(ctx, cache, in, out)
}

func Serve(ctx context.Context, cache *ArchiveCache, in io.Reader, out io.Writer) (err error) {
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)

	if err := enc.Encode(Response{KnownCommands: []Cmd{CmdGet, CmdPut, CmdClose}}); err != nil {
		return err
	}

	respCh := make(chan *Response, 64)
	writerDone := make(chan error, 1)
	go func() {
		var werr error
		for resp := range respCh {
			if werr != nil {
				continue
			}
			werr = enc.Encode(resp)
		}
		writerDone <- werr
	}()

	var inflight sync.WaitGroup
	defer func() {
		inflight.Wait()
		close(respCh)
		if werr := <-writerDone; werr != nil && err == nil {
			err = werr
		}
	}()

	for {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}

		var req Request
		if derr := dec.Decode(&req); derr != nil {
			if errors.Is(derr, io.EOF) {
				return nil
			}
			return derr
		}

		switch req.Command {
		case CmdGet:
			inflight.Add(1)
			go func(r Request) {
				defer inflight.Done()
				respCh <- handleGet(cache, &r)
			}(req)
		case CmdPut:
			var body []byte
			if req.BodySize > 0 {
				if derr := dec.Decode(&body); derr != nil {
					return derr
				}
			}
			respCh <- handlePut(cache, &req, body)
		case CmdClose:
			inflight.Wait()
			respCh <- handleClose(cache, &req)
			return nil
		default:
			respCh <- &Response{ID: req.ID, Err: fmt.Sprintf("unknown command: %q", req.Command)}
		}
	}
}

func handleGet(cache *ArchiveCache, req *Request) *Response {
	resp := &Response{ID: req.ID}
	entry, err := cache.Get(req.ActionID)
	if err != nil {
		resp.Err = err.Error()
		return resp
	}
	if entry == nil {
		resp.Miss = true
		return resp
	}
	t := entry.Time
	resp.OutputID = entry.OutputID
	resp.Size = entry.Size
	resp.Time = &t
	resp.DiskPath = entry.DiskPath
	return resp
}

func handlePut(cache *ArchiveCache, req *Request, body []byte) *Response {
	resp := &Response{ID: req.ID}
	outputID := req.OutputID
	if len(outputID) == 0 {
		outputID = req.ObjectID
	}
	path, err := cache.Put(req.ActionID, outputID, req.BodySize, body)
	if err != nil {
		resp.Err = err.Error()
		return resp
	}
	resp.DiskPath = path
	return resp
}

func handleClose(cache *ArchiveCache, req *Request) *Response {
	resp := &Response{ID: req.ID}
	if err := cache.Flush(); err != nil {
		resp.Err = err.Error()
	}
	return resp
}
