package session

import "crypto/tls"

func tlsState() *tls.ConnectionState { return &tls.ConnectionState{} }
