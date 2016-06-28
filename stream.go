package kuberesolver

import (
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"sync"

	"google.golang.org/grpc/grpclog"
)

// Decoder allows StreamWatcher to watch any stream for which a Decoder can be written.
type decoder interface {
	// Decode should return the type of event, the decoded object, or an error.
	// An error will cause StreamWatcher to call Close(). Decode should block until
	// it has data or an error occurs.
	Decode() (object Event, err error)

	// Close should close the underlying io.Reader, signalling to the source of
	// the stream that it is no longer being watched. Close() must cause any
	// outstanding call to Decode() to return with an error of some sort.
	Close()
}

// Interface can be implemented by anything that knows how to watch and report changes.
type watchInterface interface {
	// Stops watching. Will close the channel returned by ResultChan(). Releases
	// any resources used by the watch.
	Stop()

	// Returns a chan which will receive all the events. If an error occurs
	// or Stop() is called, this channel will be closed, in which case the
	// watch should be completely cleaned up.
	ResultChan() <-chan Event
}

// Decoder implements the watch.Decoder interface for io.ReadClosers that
// have contents which consist of a series of watchEvent objects encoded via JSON.
// It will decode any object registered in the supplied codec.
type JSONDecoder struct {
	r       io.ReadCloser
	decoder *json.Decoder
}

// NewDecoder creates an Decoder for the given writer and codec.
func newDecoder(r io.ReadCloser) *JSONDecoder {
	return &JSONDecoder{
		r:       r,
		decoder: json.NewDecoder(r),
	}
}

// Decode blocks until it can return the next object in the writer. Returns an error
// if the writer is closed or an object can't be decoded.
func (d *JSONDecoder) Decode() (Event, error) {
	var got Event
	if err := d.decoder.Decode(&got); err != nil {
		return Event{}, err
	}
	return got, nil
}

// Close closes the underlying r.
func (d *JSONDecoder) Close() {
	d.r.Close()
}

// StreamWatcher turns any stream for which you can write a Decoder interface
// into a watch.Interface.
type streamWatcher struct {
	source  decoder
	result  chan Event
	sync.Mutex
	stopped bool
}

// NewStreamWatcher creates a StreamWatcher from the given decoder.
func newStreamWatcher(r io.ReadCloser) watchInterface {
	sw := &streamWatcher{
		source: newDecoder(r),
		// It's easy for a consumer to add buffering via an extra
		// goroutine/channel, but impossible for them to remove it,
		// so nonbuffered is better.
		result: make(chan Event),
	}
	go sw.receive()
	return sw
}

// ResultChan implements Interface.
func (sw *streamWatcher) ResultChan() <-chan Event {
	return sw.result
}

// Stop implements Interface.
func (sw *streamWatcher) Stop() {
	// Call Close() exactly once by locking and setting a flag.
	sw.Lock()
	defer sw.Unlock()
	if !sw.stopped {
		sw.stopped = true
		sw.source.Close()
	}
}

// stopping returns true if Stop() was called previously.
func (sw *streamWatcher) stopping() bool {
	sw.Lock()
	defer sw.Unlock()
	return sw.stopped
}

// IsProbableEOF returns true if the given error resembles a connection termination
// scenario that would justify assuming that the watch is empty.
// These errors are what the Go http stack returns back to us which are general
// connection closure errors (strongly correlated) and callers that need to
// differentiate probable errors in connection behavior between normal "this is
// disconnected" should use the method.
func isProbableEOF(err error) bool {
	if uerr, ok := err.(*url.Error); ok {
		err = uerr.Err
	}
	switch {
	case err == io.EOF:
		return true
	case err.Error() == "http: can't write HTTP request on broken connection":
		return true
	case strings.Contains(err.Error(), "connection reset by peer"):
		return true
	case strings.Contains(strings.ToLower(err.Error()), "use of closed network connection"):
		return true
	}
	return false
}

// receive reads result from the decoder in a loop and sends down the result channel.
func (sw *streamWatcher) receive() {
	defer close(sw.result)
	defer sw.Stop()
	for {
		obj, err := sw.source.Decode()
		if err != nil {
			// Ignore expected error.
			if sw.stopping() {
				return
			}
			switch err {
			case io.EOF:
			// watch closed normally
			case io.ErrUnexpectedEOF:
				grpclog.Printf("kuberesolver/stream.go: Unexpected EOF during watch stream event decoding: %v", err)
			default:
				msg := "kuberesolver/stream.go: Unable to decode an event from the watch stream: %v"
				if isProbableEOF(err) {
					grpclog.Printf(msg, err)
				} else {
					grpclog.Printf(msg, err)
				}
			}
			return
		}
		sw.result <- obj
	}
}
