package redact

import "log"

// ErrorLog returns a logger for http.Server.ErrorLog. net/http prints a handler
// panic through it (the panic value and the stack), and a panic outside gin's
// Recovery (the request logger, the gate, the control mux) can carry a URL with
// a token. Each line goes through String and then to the std logger's current
// output, looked up at write time.
func ErrorLog() *log.Logger {
	return log.New(writer{}, "", 0)
}

type writer struct{}

func (writer) Write(p []byte) (int, error) {
	// log.Output adds the std logger's own prefix and flags to the line.
	if err := log.Output(0, String(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
