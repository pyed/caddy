package httpserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tlsHandler is a http.Handler that will inject a value
// into the request context indicating if the TLS
// connection is likely being intercepted.
type tlsHandler struct {
	next        http.Handler
	listener    *tlsHelloListener
	closeOnMITM bool // whether to close connection on MITM; TODO: expose through new directive
}

// ServeHTTP checks the User-Agent. For the four main browsers (Chrome,
// Edge, Firefox, and Safari) indicated by the User-Agent, the properties
// of the TLS Client Hello will be compared. The context value "mitm" will
// be set to a value indicating if it is likely that the underlying TLS
// connection is being intercepted.
//
// Note that due to Microsoft's decision to intentionally make IE/Edge
// user agents obscure (and look like other browsers), this may offer
// less accuracy for IE/Edge clients.
//
// This MITM detection capability is based on research done by Durumeric,
// Halderman, et. al. in "The Security Impact of HTTPS Interception" (NDSS '17):
// https://jhalderm.com/pub/papers/interception-ndss17.pdf
func (h *tlsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.listener.helloInfosMu.RLock()
	info := h.listener.helloInfos[r.RemoteAddr]
	h.listener.helloInfosMu.RUnlock()

	ua := r.Header.Get("User-Agent")

	var checked, mitm bool
	if r.Header.Get("X-BlueCoat-Via") != "" || // Blue Coat (masks User-Agent header to generic values)
		r.Header.Get("X-FCCKV2") != "" || // Fortinet
		info.advertisesHeartbeatSupport() { // no major browsers have ever implemented Heartbeat
		checked = true
		mitm = true
	} else if strings.Contains(ua, "Edge") || strings.Contains(ua, "MSIE") ||
		strings.Contains(ua, "Trident") {
		checked = true
		mitm = !info.looksLikeEdge()
	} else if strings.Contains(ua, "Chrome") {
		checked = true
		mitm = !info.looksLikeChrome()
	} else if strings.Contains(ua, "Firefox") {
		checked = true
		mitm = !info.looksLikeFirefox()
	} else if strings.Contains(ua, "Safari") {
		checked = true
		mitm = !info.looksLikeSafari()
	}

	if checked {
		r = r.WithContext(context.WithValue(r.Context(), CtxKey("mitm"), mitm))
	}

	if mitm && h.closeOnMITM {
		// TODO: This termination might need to happen later in the middleware
		// chain in order to be picked up by the log directive, in case the site
		// owner still wants to log this event. It'll probably require a new
		// directive. If this feature is useful, we can finish implementing this.
		r.Close = true
		return
	}

	h.next.ServeHTTP(w, r)
}

type clientHelloConn struct {
	net.Conn
	readHello bool
	listener  *tlsHelloListener
}

func (c *clientHelloConn) Read(b []byte) (n int, err error) {
	if !c.readHello {
		// Read the header bytes.
		hdr := make([]byte, 5)
		n, err := io.ReadFull(c.Conn, hdr)
		if err != nil {
			return n, err
		}

		// Get the length of the ClientHello message and read it as well.
		length := uint16(hdr[3])<<8 | uint16(hdr[4])
		hello := make([]byte, int(length))
		n, err = io.ReadFull(c.Conn, hello)
		if err != nil {
			return n, err
		}

		// Parse the ClientHello and store it in the map.
		rawParsed := parseRawClientHello(hello)
		c.listener.helloInfosMu.Lock()
		c.listener.helloInfos[c.Conn.RemoteAddr().String()] = rawParsed
		c.listener.helloInfosMu.Unlock()

		// Since we buffered the header and ClientHello, pretend we were
		// never here by lining up the buffered values to be read with a
		// custom connection type, followed by the rest of the actual
		// underlying connection.
		mr := io.MultiReader(bytes.NewReader(hdr), bytes.NewReader(hello), c.Conn)
		mc := multiConn{Conn: c.Conn, reader: mr}

		c.Conn = mc

		c.readHello = true
	}
	return c.Conn.Read(b)
}

// multiConn is a net.Conn that reads from the
// given reader instead of the wire directly. This
// is useful when some of the connection has already
// been read (like the TLS Client Hello) and the
// reader is a io.MultiReader that starts with
// the contents of the buffer.
type multiConn struct {
	net.Conn
	reader io.Reader
}

// Read reads from mc.reader.
func (mc multiConn) Read(b []byte) (n int, err error) {
	return mc.reader.Read(b)
}

// parseRawClientHello parses data which contains the raw
// TLS Client Hello message. It extracts relevant information
// into info. Any error reading the Client Hello (such as
// insufficient length or invalid length values) results in
// a silent error and an incomplete info struct, since there
// is no good way to handle an error like this during Accept().
// The data is expected to contain the whole ClientHello and
// ONLY the ClientHello.
//
// The majority of this code is borrowed from the Go standard
// library, which is (c) The Go Authors. It has been modified
// to fit this use case.
func parseRawClientHello(data []byte) (info rawHelloInfo) {
	if len(data) < 42 {
		return
	}
	sessionIdLen := int(data[38])
	if sessionIdLen > 32 || len(data) < 39+sessionIdLen {
		return
	}
	data = data[39+sessionIdLen:]
	if len(data) < 2 {
		return
	}
	// cipherSuiteLen is the number of bytes of cipher suite numbers. Since
	// they are uint16s, the number must be even.
	cipherSuiteLen := int(data[0])<<8 | int(data[1])
	if cipherSuiteLen%2 == 1 || len(data) < 2+cipherSuiteLen {
		return
	}
	numCipherSuites := cipherSuiteLen / 2
	// read in the cipher suites
	info.cipherSuites = make([]uint16, numCipherSuites)
	for i := 0; i < numCipherSuites; i++ {
		info.cipherSuites[i] = uint16(data[2+2*i])<<8 | uint16(data[3+2*i])
	}
	data = data[2+cipherSuiteLen:]
	if len(data) < 1 {
		return
	}
	// read in the compression methods
	compressionMethodsLen := int(data[0])
	if len(data) < 1+compressionMethodsLen {
		return
	}
	info.compressionMethods = data[1 : 1+compressionMethodsLen]

	data = data[1+compressionMethodsLen:]

	// ClientHello is optionally followed by extension data
	if len(data) < 2 {
		return
	}
	extensionsLength := int(data[0])<<8 | int(data[1])
	data = data[2:]
	if extensionsLength != len(data) {
		return
	}

	// read in each extension, and extract any relevant information
	// from extensions we care about
	for len(data) != 0 {
		if len(data) < 4 {
			return
		}
		extension := uint16(data[0])<<8 | uint16(data[1])
		length := int(data[2])<<8 | int(data[3])
		data = data[4:]
		if len(data) < length {
			return
		}

		// record that the client advertised support for this extension
		info.extensions = append(info.extensions, extension)

		switch extension {
		case extensionSupportedCurves:
			// http://tools.ietf.org/html/rfc4492#section-5.5.1
			if length < 2 {
				return
			}
			l := int(data[0])<<8 | int(data[1])
			if l%2 == 1 || length != l+2 {
				return
			}
			numCurves := l / 2
			info.curves = make([]tls.CurveID, numCurves)
			d := data[2:]
			for i := 0; i < numCurves; i++ {
				info.curves[i] = tls.CurveID(d[0])<<8 | tls.CurveID(d[1])
				d = d[2:]
			}
		case extensionSupportedPoints:
			// http://tools.ietf.org/html/rfc4492#section-5.5.2
			if length < 1 {
				return
			}
			l := int(data[0])
			if length != l+1 {
				return
			}
			info.points = make([]uint8, l)
			copy(info.points, data[1:])
		}

		data = data[length:]
	}

	return
}

// newTLSListener returns a new tlsHelloListener that wraps ln.
func newTLSListener(ln net.Listener, config *tls.Config, readTimeout time.Duration) *tlsHelloListener {
	return &tlsHelloListener{
		Listener:    ln,
		config:      config,
		readTimeout: readTimeout,
		helloInfos:  make(map[string]rawHelloInfo),
	}
}

// tlsHelloListener is a TLS listener that is specially designed
// to read the ClientHello manually so we can extract necessary
// information from it. Each ClientHello message is mapped by
// the remote address of the client, which must be removed when
// the connection is closed (use ConnState).
type tlsHelloListener struct {
	net.Listener
	config       *tls.Config
	readTimeout  time.Duration
	helloInfos   map[string]rawHelloInfo
	helloInfosMu sync.RWMutex
}

// Accept waits for and returns the next connection to the listener.
// After it accepts the underlying connection, it reads the
// ClientHello message and stores the parsed data into a map on l.
func (l *tlsHelloListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	helloConn := &clientHelloConn{Conn: conn, listener: l}
	return tls.Server(helloConn, l.config), nil
}

// rawHelloInfo contains the "raw" data parsed from the TLS
// Client Hello. No interpretation is done on the raw data.
//
// The methods on this type implement heuristics described
// by Durumeric, Halderman, et. al. in
// "The Security Impact of HTTPS Interception":
// https://jhalderm.com/pub/papers/interception-ndss17.pdf
type rawHelloInfo struct {
	cipherSuites       []uint16
	extensions         []uint16
	compressionMethods []byte
	curves             []tls.CurveID
	points             []uint8
}

// advertisesHeartbeatSupport returns true if info indicates
// that the client supports the Heartbeat extension.
func (info rawHelloInfo) advertisesHeartbeatSupport() bool {
	for _, ext := range info.extensions {
		if ext == extensionHeartbeat {
			return true
		}
	}
	return false
}

// looksLikeFirefox returns true if info looks like a handshake
// from a modern version of Firefox.
func (info rawHelloInfo) looksLikeFirefox() bool {
	// "To determine whether a Firefox session has been
	// intercepted, we check for the presence and order
	// of extensions, cipher suites, elliptic curves,
	// EC point formats, and handshake compression methods."

	// We check for the presence and order of the extensions.
	// Note: Sometimes padding (21) is present, sometimes not.
	// Note: Firefox 51+ does not advertise 0x3374 (13172, NPN).
	// Note: Firefox doesn't advertise 0x0 (0, SNI) when connecting to IP addresses.
	requiredExtensionsOrder := []uint16{23, 65281, 10, 11, 35, 16, 5, 65283, 13}
	if !assertPresenceAndOrdering(requiredExtensionsOrder, info.extensions, true) {
		return false
	}

	// We check for both presence of curves and their ordering.
	expectedCurves := []tls.CurveID{29, 23, 24, 25}
	if len(info.curves) != len(expectedCurves) {
		return false
	}
	for i := range expectedCurves {
		if info.curves[i] != expectedCurves[i] {
			return false
		}
	}

	// We check for order of cipher suites but not presence, since
	// according to the paper, cipher suites may be not be added
	// or reordered by the user, but they may be disabled.
	expectedCipherSuiteOrder := []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, // 0xc02b
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,   // 0xc02f
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,  // 0xcca9
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,    // 0xcca8
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, // 0xc02c
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,   // 0xc030
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,    // 0xc00a
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,    // 0xc009
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,      // 0xc013
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,      // 0xc014
		TLS_DHE_RSA_WITH_AES_128_CBC_SHA,            // 0x33
		TLS_DHE_RSA_WITH_AES_256_CBC_SHA,            // 0x39
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,            // 0x2f
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,            // 0x35
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,           // 0xa
	}
	return assertPresenceAndOrdering(expectedCipherSuiteOrder, info.cipherSuites, false)
}

// looksLikeChrome returns true if info looks like a handshake
// from a modern version of Chrome.
func (info rawHelloInfo) looksLikeChrome() bool {
	// "We check for ciphers and extensions that Chrome is known
	// to not support, but do not check for the inclusion of
	// specific ciphers or extensions, nor do we validate their
	// order. When appropriate, we check the presence and order
	// of elliptic curves, compression methods, and EC point formats."

	// Not in Chrome 56, but present in Safari 10 (Feb. 2017):
	// TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA384 (0xc024)
	// TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256 (0xc023)
	// TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA (0xc00a)
	// TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA (0xc009)
	// TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA384 (0xc028)
	// TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256 (0xc027)
	// TLS_RSA_WITH_AES_256_CBC_SHA256 (0x3d)
	// TLS_RSA_WITH_AES_128_CBC_SHA256 (0x3c)

	// Not in Chrome 56, but present in Firefox 51 (Feb. 2017):
	// TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA (0xc00a)
	// TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA (0xc009)
	// TLS_DHE_RSA_WITH_AES_128_CBC_SHA (0x33)
	// TLS_DHE_RSA_WITH_AES_256_CBC_SHA (0x39)

	// Selected ciphers present in Chrome mobile (Feb. 2017):
	// 0xc00a, 0xc014, 0xc009, 0x9c, 0x9d, 0x2f, 0x35, 0xa

	chromeCipherExclusions := map[uint16]struct{}{
		TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA384:   {}, // 0xc024
		TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256:   {}, // 0xc023
		TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA384:     {}, // 0xc028
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256: {}, // 0xc027
		TLS_RSA_WITH_AES_256_CBC_SHA256:           {}, // 0x3d
		tls.TLS_RSA_WITH_AES_128_CBC_SHA256:       {}, // 0x3c
		TLS_DHE_RSA_WITH_AES_128_CBC_SHA:          {}, // 0x33
		TLS_DHE_RSA_WITH_AES_256_CBC_SHA:          {}, // 0x39
	}
	for _, ext := range info.cipherSuites {
		if _, ok := chromeCipherExclusions[ext]; ok {
			return false
		}
	}

	// Chrome does not include curve 25 (CurveP521) (as of Chrome 56, Feb. 2017).
	for _, curve := range info.curves {
		if curve == 25 {
			return false
		}
	}

	return true
}

// looksLikeEdge returns true if info looks like a handshake
// from a modern version of MS Edge.
func (info rawHelloInfo) looksLikeEdge() bool {
	// "SChannel connections can by uniquely identified because SChannel
	// is the only TLS library we tested that includes the OCSP status
	// request extension before the supported groups and EC point formats
	// extensions."
	//
	// More specifically, the OCSP status request extension appears
	// *directly* before the other two extensions, which occur in that
	// order. (I contacted the authors for clarification and verified it.)
	for i, ext := range info.extensions {
		if ext == extensionOCSPStatusRequest {
			if len(info.extensions) <= i+2 {
				return false
			}
			if info.extensions[i+1] != extensionSupportedCurves ||
				info.extensions[i+2] != extensionSupportedPoints {
				return false
			}
		}
	}

	for _, cs := range info.cipherSuites {
		// As of Feb. 2017, Edge does not have 0xff, but Avast adds it
		if cs == scsvRenegotiation {
			return false
		}
		// Edge and modern IE do not have 0x4 or 0x5, but Blue Coat does
		if cs == TLS_RSA_WITH_RC4_128_MD5 || cs == tls.TLS_RSA_WITH_RC4_128_SHA {
			return false
		}
	}

	return true
}

// looksLikeSafari returns true if info looks like a handshake
// from a modern version of MS Safari.
func (info rawHelloInfo) looksLikeSafari() bool {
	// "One unique aspect of Secure Transport is that it includes
	// the TLS_EMPTY_RENEGOTIATION_INFO_SCSV (0xff) cipher first,
	// whereas the other libraries we investigated include the
	// cipher last. Similar to Microsoft, Apple has changed
	// TLS behavior in minor OS updates, which are not indicated
	// in the HTTP User-Agent header. We allow for any of the
	// updates when validating handshakes, and we check for the
	// presence and ordering of ciphers, extensions, elliptic
	// curves, and compression methods."

	// Note that any C lib (e.g. curl) compiled on macOS
	// will probably use Secure Transport which will also
	// share the TLS handshake characteristics of Safari.

	// Let's do the easy check first... should be sufficient in many cases.
	if len(info.cipherSuites) < 1 {
		return false
	}
	if info.cipherSuites[0] != scsvRenegotiation {
		return false
	}

	// We check for the presence and order of the extensions.
	requiredExtensionsOrder := []uint16{10, 11, 13, 13172, 16, 5, 18, 23}
	if !assertPresenceAndOrdering(requiredExtensionsOrder, info.extensions, true) {
		return false
	}

	// We check for order and presence of cipher suites
	expectedCipherSuiteOrder := []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, // 0xc02c
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, // 0xc02b
		TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA384,     // 0xc024
		TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,     // 0xc023
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,    // 0xc00a
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,    // 0xc009
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,   // 0xc030
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,   // 0xc02f
		TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA384,       // 0xc028
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,   // 0xc027
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,      // 0xc014
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,      // 0xc013
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,         // 0x9d
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256,         // 0x9c
		TLS_RSA_WITH_AES_256_CBC_SHA256,             // 0x3d
		TLS_RSA_WITH_AES_128_CBC_SHA256,             // 0x3c
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,            // 0x35
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,            // 0x2f
	}
	return assertPresenceAndOrdering(expectedCipherSuiteOrder, info.cipherSuites, true)
}

// assertPresenceAndOrdering will return true if candidateList contains
// the items in requiredItems in the same order as requiredItems.
//
// If requiredIsSubset is true, then all items in requiredItems must be
// present in candidateList. If requiredIsSubset is false, then requiredItems
// may contain items that are not in candidateList.
//
// In all cases, the order of requiredItems is enforced.
func assertPresenceAndOrdering(requiredItems, candidateList []uint16, requiredIsSubset bool) bool {
	superset := requiredItems
	subset := candidateList
	if requiredIsSubset {
		superset = candidateList
		subset = requiredItems
	}

	var j int
	for _, item := range subset {
		var found bool
		for j < len(superset) {
			if superset[j] == item {
				found = true
				break
			}
			j++
		}
		if j == len(superset) && !found {
			return false
		}
	}
	return true
}

const (
	extensionOCSPStatusRequest = 5
	extensionSupportedCurves   = 10 // also called "SupportedGroups"
	extensionSupportedPoints   = 11
	extensionHeartbeat         = 15

	scsvRenegotiation = 0xff

	// cipher suites missing from the crypto/tls package,
	// in no particular order here
	TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA384 = 0xc024
	TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256 = 0xc023
	TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA384   = 0xc028
	TLS_RSA_WITH_AES_128_CBC_SHA256         = 0x3c
	TLS_RSA_WITH_AES_256_CBC_SHA256         = 0x3d
	TLS_DHE_RSA_WITH_AES_128_CBC_SHA        = 0x33
	TLS_DHE_RSA_WITH_AES_256_CBC_SHA        = 0x39
	TLS_RSA_WITH_RC4_128_MD5                = 0x4
)
