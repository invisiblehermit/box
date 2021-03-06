package docker

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/containers/image/docker/reference"
	"github.com/containers/image/types"
	"github.com/containers/storage/pkg/homedir"
	"github.com/docker/go-connections/sockets"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/pkg/errors"
)

const (
	dockerHostname     = "docker.io"
	dockerRegistry     = "registry-1.docker.io"
	dockerAuthRegistry = "https://index.docker.io/v1/"

	dockerCfg         = ".docker"
	dockerCfgFileName = "config.json"
	dockerCfgObsolete = ".dockercfg"

	baseURL       = "%s://%s/v2/"
	baseURLV1     = "%s://%s/v1/_ping"
	tagsURL       = "%s/tags/list"
	manifestURL   = "%s/manifests/%s"
	blobsURL      = "%s/blobs/%s"
	blobUploadURL = "%s/blobs/uploads/"

	minimumTokenLifetimeSeconds = 60
)

// ErrV1NotSupported is returned when we're trying to talk to a
// docker V1 registry.
var ErrV1NotSupported = errors.New("can't talk to a V1 docker registry")

type bearerToken struct {
	Token     string    `json:"token"`
	ExpiresIn int       `json:"expires_in"`
	IssuedAt  time.Time `json:"issued_at"`
}

// dockerClient is configuration for dealing with a single Docker registry.
type dockerClient struct {
	ctx             *types.SystemContext
	registry        string
	username        string
	password        string
	scheme          string // Cache of a value returned by a successful ping() if not empty
	client          *http.Client
	signatureBase   signatureStorageBase
	challenges      []challenge
	scope           authScope
	token           *bearerToken
	tokenExpiration time.Time
}

type authScope struct {
	remoteName string
	actions    string
}

// this is cloned from docker/go-connections because upstream docker has changed
// it and make deps here fails otherwise.
// We'll drop this once we upgrade to docker 1.13.x deps.
func serverDefault() *tls.Config {
	return &tls.Config{
		// Avoid fallback to SSL protocols < TLS1.0
		MinVersion:               tls.VersionTLS10,
		PreferServerCipherSuites: true,
		CipherSuites:             tlsconfig.DefaultServerAcceptedCiphers,
	}
}

func newTransport() *http.Transport {
	direct := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		DualStack: true,
	}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		Dial:                direct.Dial,
		TLSHandshakeTimeout: 10 * time.Second,
		// TODO(dmcgowan): Call close idle connections when complete and use keep alive
		DisableKeepAlives: true,
	}
	proxyDialer, err := sockets.DialerFromEnvironment(direct)
	if err == nil {
		tr.Dial = proxyDialer.Dial
	}
	return tr
}

func setupCertificates(dir string, tlsc *tls.Config) error {
	if dir == "" {
		return nil
	}
	fs, err := ioutil.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	for _, f := range fs {
		fullPath := filepath.Join(dir, f.Name())
		if strings.HasSuffix(f.Name(), ".crt") {
			tlsc.RootCAs = tlsconfig.ClientDefault().RootCAs
			logrus.Debugf("crt: %s", fullPath)
			data, err := ioutil.ReadFile(fullPath)
			if err != nil {
				return err
			}
			tlsc.RootCAs.AppendCertsFromPEM(data)
		}
		if strings.HasSuffix(f.Name(), ".cert") {
			certName := f.Name()
			keyName := certName[:len(certName)-5] + ".key"
			logrus.Debugf("cert: %s", fullPath)
			if !hasFile(fs, keyName) {
				return errors.Errorf("missing key %s for client certificate %s. Note that CA certificates should use the extension .crt", keyName, certName)
			}
			cert, err := tls.LoadX509KeyPair(filepath.Join(dir, certName), filepath.Join(dir, keyName))
			if err != nil {
				return err
			}
			tlsc.Certificates = append(tlsc.Certificates, cert)
		}
		if strings.HasSuffix(f.Name(), ".key") {
			keyName := f.Name()
			certName := keyName[:len(keyName)-4] + ".cert"
			logrus.Debugf("key: %s", fullPath)
			if !hasFile(fs, certName) {
				return errors.Errorf("missing client certificate %s for key %s", certName, keyName)
			}
		}
	}
	return nil
}

func hasFile(files []os.FileInfo, name string) bool {
	for _, f := range files {
		if f.Name() == name {
			return true
		}
	}
	return false
}

// newDockerClient returns a new dockerClient instance for refHostname (a host a specified in the Docker image reference, not canonicalized to dockerRegistry)
// “write” specifies whether the client will be used for "write" access (in particular passed to lookaside.go:toplevelFromSection)
func newDockerClient(ctx *types.SystemContext, ref dockerReference, write bool, actions string) (*dockerClient, error) {
	registry := reference.Domain(ref.ref)
	if registry == dockerHostname {
		registry = dockerRegistry
	}
	username, password, err := getAuth(ctx, reference.Domain(ref.ref))
	if err != nil {
		return nil, err
	}
	tr := newTransport()
	if ctx != nil && (ctx.DockerCertPath != "" || ctx.DockerInsecureSkipTLSVerify) {
		tlsc := &tls.Config{}

		if err := setupCertificates(ctx.DockerCertPath, tlsc); err != nil {
			return nil, err
		}

		tlsc.InsecureSkipVerify = ctx.DockerInsecureSkipTLSVerify
		tr.TLSClientConfig = tlsc
	}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = serverDefault()
	}
	client := &http.Client{Transport: tr}

	sigBase, err := configuredSignatureStorageBase(ctx, ref, write)
	if err != nil {
		return nil, err
	}

	return &dockerClient{
		ctx:           ctx,
		registry:      registry,
		username:      username,
		password:      password,
		client:        client,
		signatureBase: sigBase,
		scope: authScope{
			actions:    actions,
			remoteName: reference.Path(ref.ref),
		},
	}, nil
}

// makeRequest creates and executes a http.Request with the specified parameters, adding authentication and TLS options for the Docker client.
// url is NOT an absolute URL, but a path relative to the /v2/ top-level API path.  The host name and schema is taken from the client or autodetected.
func (c *dockerClient) makeRequest(method, url string, headers map[string][]string, stream io.Reader) (*http.Response, error) {
	if c.scheme == "" {
		if err := c.ping(); err != nil {
			return nil, err
		}
	}

	url = fmt.Sprintf(baseURL, c.scheme, c.registry) + url
	return c.makeRequestToResolvedURL(method, url, headers, stream, -1, true)
}

// makeRequestToResolvedURL creates and executes a http.Request with the specified parameters, adding authentication and TLS options for the Docker client.
// streamLen, if not -1, specifies the length of the data expected on stream.
// makeRequest should generally be preferred.
// TODO(runcom): too many arguments here, use a struct
func (c *dockerClient) makeRequestToResolvedURL(method, url string, headers map[string][]string, stream io.Reader, streamLen int64, sendAuth bool) (*http.Response, error) {
	req, err := http.NewRequest(method, url, stream)
	if err != nil {
		return nil, err
	}
	if streamLen != -1 { // Do not blindly overwrite if streamLen == -1, http.NewRequest above can figure out the length of bytes.Reader and similar objects without us having to compute it.
		req.ContentLength = streamLen
	}
	req.Header.Set("Docker-Distribution-API-Version", "registry/2.0")
	for n, h := range headers {
		for _, hh := range h {
			req.Header.Add(n, hh)
		}
	}
	if c.ctx != nil && c.ctx.DockerRegistryUserAgent != "" {
		req.Header.Add("User-Agent", c.ctx.DockerRegistryUserAgent)
	}
	if sendAuth {
		if err := c.setupRequestAuth(req); err != nil {
			return nil, err
		}
	}
	logrus.Debugf("%s %s", method, url)
	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// we're using the challenges from the /v2/ ping response and not the one from the destination
// URL in this request because:
//
// 1) docker does that as well
// 2) gcr.io is sending 401 without a WWW-Authenticate header in the real request
//
// debugging: https://github.com/containers/image/pull/211#issuecomment-273426236 and follows up
func (c *dockerClient) setupRequestAuth(req *http.Request) error {
	if len(c.challenges) == 0 {
		return nil
	}
	// assume just one...
	challenge := c.challenges[0]
	switch challenge.Scheme {
	case "basic":
		req.SetBasicAuth(c.username, c.password)
		return nil
	case "bearer":
		if c.token == nil || time.Now().After(c.tokenExpiration) {
			realm, ok := challenge.Parameters["realm"]
			if !ok {
				return errors.Errorf("missing realm in bearer auth challenge")
			}
			service, _ := challenge.Parameters["service"] // Will be "" if not present
			scope := fmt.Sprintf("repository:%s:%s", c.scope.remoteName, c.scope.actions)
			token, err := c.getBearerToken(realm, service, scope)
			if err != nil {
				return err
			}
			c.token = token
			c.tokenExpiration = token.IssuedAt.Add(time.Duration(token.ExpiresIn) * time.Second)
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token.Token))
		return nil
	}
	return errors.Errorf("no handler for %s authentication", challenge.Scheme)
}

func (c *dockerClient) getBearerToken(realm, service, scope string) (*bearerToken, error) {
	authReq, err := http.NewRequest("GET", realm, nil)
	if err != nil {
		return nil, err
	}
	getParams := authReq.URL.Query()
	if service != "" {
		getParams.Add("service", service)
	}
	if scope != "" {
		getParams.Add("scope", scope)
	}
	authReq.URL.RawQuery = getParams.Encode()
	if c.username != "" && c.password != "" {
		authReq.SetBasicAuth(c.username, c.password)
	}
	tr := newTransport()
	// TODO(runcom): insecure for now to contact the external token service
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	client := &http.Client{Transport: tr}
	res, err := client.Do(authReq)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusUnauthorized:
		return nil, errors.Errorf("unable to retrieve auth token: 401 unauthorized")
	case http.StatusOK:
		break
	default:
		return nil, errors.Errorf("unexpected http code: %d, URL: %s", res.StatusCode, authReq.URL)
	}
	tokenBlob, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	var token bearerToken
	if err := json.Unmarshal(tokenBlob, &token); err != nil {
		return nil, err
	}
	if token.ExpiresIn < minimumTokenLifetimeSeconds {
		token.ExpiresIn = minimumTokenLifetimeSeconds
		logrus.Debugf("Increasing token expiration to: %d seconds", token.ExpiresIn)
	}
	if token.IssuedAt.IsZero() {
		token.IssuedAt = time.Now().UTC()
	}
	return &token, nil
}

func getAuth(ctx *types.SystemContext, registry string) (string, string, error) {
	if ctx != nil && ctx.DockerAuthConfig != nil {
		return ctx.DockerAuthConfig.Username, ctx.DockerAuthConfig.Password, nil
	}
	var dockerAuth dockerConfigFile
	dockerCfgPath := filepath.Join(getDefaultConfigDir(".docker"), dockerCfgFileName)
	if _, err := os.Stat(dockerCfgPath); err == nil {
		j, err := ioutil.ReadFile(dockerCfgPath)
		if err != nil {
			return "", "", err
		}
		if err := json.Unmarshal(j, &dockerAuth); err != nil {
			return "", "", err
		}

	} else if os.IsNotExist(err) {
		// try old config path
		oldDockerCfgPath := filepath.Join(getDefaultConfigDir(dockerCfgObsolete))
		if _, err := os.Stat(oldDockerCfgPath); err != nil {
			if os.IsNotExist(err) {
				return "", "", nil
			}
			return "", "", errors.Wrap(err, oldDockerCfgPath)
		}

		j, err := ioutil.ReadFile(oldDockerCfgPath)
		if err != nil {
			return "", "", err
		}
		if err := json.Unmarshal(j, &dockerAuth.AuthConfigs); err != nil {
			return "", "", err
		}

	} else if err != nil {
		return "", "", errors.Wrap(err, dockerCfgPath)
	}

	// I'm feeling lucky
	if c, exists := dockerAuth.AuthConfigs[registry]; exists {
		return decodeDockerAuth(c.Auth)
	}

	// bad luck; let's normalize the entries first
	registry = normalizeRegistry(registry)
	normalizedAuths := map[string]dockerAuthConfig{}
	for k, v := range dockerAuth.AuthConfigs {
		normalizedAuths[normalizeRegistry(k)] = v
	}
	if c, exists := normalizedAuths[registry]; exists {
		return decodeDockerAuth(c.Auth)
	}
	return "", "", nil
}

func (c *dockerClient) ping() error {
	ping := func(scheme string) error {
		url := fmt.Sprintf(baseURL, scheme, c.registry)
		resp, err := c.makeRequestToResolvedURL("GET", url, nil, nil, -1, true)
		logrus.Debugf("Ping %s err %#v", url, err)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		logrus.Debugf("Ping %s status %d", scheme+"://"+c.registry+"/v2/", resp.StatusCode)
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnauthorized {
			return errors.Errorf("error pinging repository, response code %d", resp.StatusCode)
		}
		c.challenges = parseAuthHeader(resp.Header)
		c.scheme = scheme
		return nil
	}
	err := ping("https")
	if err != nil && c.ctx != nil && c.ctx.DockerInsecureSkipTLSVerify {
		err = ping("http")
	}
	if err != nil {
		err = errors.Wrap(err, "pinging docker registry returned")
		if c.ctx != nil && c.ctx.DockerDisableV1Ping {
			return err
		}
		// best effort to understand if we're talking to a V1 registry
		pingV1 := func(scheme string) bool {
			url := fmt.Sprintf(baseURLV1, scheme, c.registry)
			resp, err := c.makeRequestToResolvedURL("GET", url, nil, nil, -1, true)
			logrus.Debugf("Ping %s err %#v", url, err)
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			logrus.Debugf("Ping %s status %d", scheme+"://"+c.registry+"/v1/_ping", resp.StatusCode)
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnauthorized {
				return false
			}
			return true
		}
		isV1 := pingV1("https")
		if !isV1 && c.ctx != nil && c.ctx.DockerInsecureSkipTLSVerify {
			isV1 = pingV1("http")
		}
		if isV1 {
			err = ErrV1NotSupported
		}
	}
	return err
}

func getDefaultConfigDir(confPath string) string {
	return filepath.Join(homedir.Get(), confPath)
}

type dockerAuthConfig struct {
	Auth string `json:"auth,omitempty"`
}

type dockerConfigFile struct {
	AuthConfigs map[string]dockerAuthConfig `json:"auths"`
}

func decodeDockerAuth(s string) (string, string, error) {
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", "", err
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		// if it's invalid just skip, as docker does
		return "", "", nil
	}
	user := parts[0]
	password := strings.Trim(parts[1], "\x00")
	return user, password, nil
}

// convertToHostname converts a registry url which has http|https prepended
// to just an hostname.
// Copied from github.com/docker/docker/registry/auth.go
func convertToHostname(url string) string {
	stripped := url
	if strings.HasPrefix(url, "http://") {
		stripped = strings.TrimPrefix(url, "http://")
	} else if strings.HasPrefix(url, "https://") {
		stripped = strings.TrimPrefix(url, "https://")
	}

	nameParts := strings.SplitN(stripped, "/", 2)

	return nameParts[0]
}

func normalizeRegistry(registry string) string {
	normalized := convertToHostname(registry)
	switch normalized {
	case "registry-1.docker.io", "docker.io":
		return "index.docker.io"
	}
	return normalized
}
