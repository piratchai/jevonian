package wire

import "errors"

// ErrStreamClosed is returned by TranslatorStream.Write after Close.
var ErrStreamClosed = errors.New("wire: stream closed")

var errEOF = errors.New("wire: EOF")

// IsEOF reports whether an error is the stream's internal EOF sentinel.
func IsEOF(err error) bool { return errors.Is(err, errEOF) }
