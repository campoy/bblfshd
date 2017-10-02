package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bblfsh/server/runtime"

	"github.com/sirupsen/logrus"
	"gopkg.in/bblfsh/sdk.v1/protocol"
	"gopkg.in/bblfsh/sdk.v1/sdk/server"
	"gopkg.in/src-d/go-errors.v1"
)

const (
	defaultTransport = "docker"
)

var (
	ErrUnexpected       = errors.NewKind("unexpected error")
	ErrMissingDriver    = errors.NewKind("missing driver for language %s")
	ErrRuntime          = errors.NewKind("runtime failure")
	ErrAlreadyInstalled = errors.NewKind("driver already installed: %s (image reference: %s)")
)

// Daemon is a Babelfish server.
type Daemon struct {
	server.Server
	// Transport to use to fetch driver images. Defaults to "docker".
	// Useful transports:
	// - docker: uses Docker registries (docker.io by default).
	// - docker-daemon: gets images from a local Docker daemon.
	Transport string
	// Overrides for images per language
	Overrides map[string]string

	version string
	runtime *runtime.Runtime
	mutex   sync.RWMutex
	pool    map[string]*DriverPool
}

// NewDaemon creates a new server based on the runtime with the given version.
func NewDaemon(version string, r *runtime.Runtime) *Daemon {
	d := &Daemon{
		version:   version,
		runtime:   r,
		pool:      make(map[string]*DriverPool),
		Overrides: make(map[string]string),
	}

	protocol.DefaultService = d
	return d
}

func (d *Daemon) AddDriver(language string, img string) error {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	if _, ok := d.pool[language]; ok {
		return ErrAlreadyInstalled.New(language, img)
	}

	image, err := runtime.NewDriverImage(img)
	if err != nil {
		return ErrRuntime.Wrap(err)
	}

	if err := d.runtime.InstallDriver(image, false); err != nil {
		return ErrRuntime.Wrap(err)
	}

	logrus.Infof("new driver installed: %q", image.Name())
	dp := NewDriverPool(func() (Driver, error) {
		logrus.Debugf("spawning driver instance %q ...", image.Name())

		opts := getDriverInstanceOptions()
		driver, err := NewDriverInstance(d.runtime, language, image, opts)
		if err != nil {
			return nil, err
		}

		if err := driver.Start(); err != nil {
			return nil, err
		}

		logrus.Infof("new driver instance started %s (%s)", image.Name(), driver.Container.ID())
		return driver, nil
	})

	dp.Logger = logrus.WithFields(logrus.Fields{
		"language": language,
	})

	d.pool[language] = dp
	return dp.Start()
}

func (d *Daemon) DriverPool(language string) (*DriverPool, error) {
	d.mutex.RLock()
	dp, ok := d.pool[language]
	d.mutex.RUnlock()

	if ok {
		return dp, nil
	}

	i := d.defaultDriverImageReference(language)
	err := d.AddDriver(language, i)
	if err != nil && !ErrAlreadyInstalled.Is(err) {
		return nil, ErrMissingDriver.Wrap(err, language)
	}

	d.mutex.RLock()
	dp = d.pool[language]
	d.mutex.RUnlock()

	return dp, nil
}

func (d *Daemon) Parse(req *protocol.ParseRequest) *protocol.ParseResponse {
	resp := &protocol.ParseResponse{}
	start := time.Now()
	defer func() {
		resp.Elapsed = time.Since(start)
		d.logResponse(resp.Status, req.Language, len(req.Content), resp.Elapsed)
	}()

	if req.Content == "" {
		return resp
	}

	language, dp, err := d.selectPool(req.Language, req.Content, req.Filename)
	if err != nil {
		logrus.Errorf("error selecting pool: %s", err)
		resp.Response = newResponseFromError(err)
		return resp
	}

	req.Language = language

	err = dp.Execute(func(driver Driver) error {
		resp, err = driver.Service().Parse(context.Background(), req)
		return err
	})

	if err != nil {
		resp = &protocol.ParseResponse{}
		resp.Response = newResponseFromError(err)
	}

	return resp
}

func (d *Daemon) logResponse(s protocol.Status, language string, size int, elapsed time.Duration) {
	l := logrus.WithFields(logrus.Fields{
		"language": language,
		"elapsed":  elapsed,
	})

	text := fmt.Sprintf("request processed content %d bytes, status %s", size, s)

	switch s {
	case protocol.Ok:
		l.Debug(text)
	case protocol.Error:
		l.Warning(text)
	case protocol.Fatal:
		l.Error(text)
	}
}

func (d *Daemon) NativeParse(req *protocol.NativeParseRequest) *protocol.NativeParseResponse {
	resp := &protocol.NativeParseResponse{}
	start := time.Now()
	defer func() {
		resp.Elapsed = time.Since(start)
		d.logResponse(resp.Status, req.Language, len(req.Content), resp.Elapsed)
	}()

	if req.Content == "" {
		logrus.Debugf("empty request received, returning empty AST")
		return resp
	}

	language, dp, err := d.selectPool(req.Language, req.Content, req.Filename)
	if err != nil {
		logrus.Errorf("error selecting pool: %s", err)
		resp.Response = newResponseFromError(err)
		return resp
	}

	req.Language = language

	err = dp.Execute(func(driver Driver) error {
		resp, err = driver.Service().NativeParse(context.Background(), req)
		return err
	})

	if err != nil {
		resp = &protocol.NativeParseResponse{}
		resp.Response = newResponseFromError(err)
	}

	return resp
}

func (d *Daemon) selectPool(language, content, filename string) (string, *DriverPool, error) {
	if language == "" {
		language = GetLanguage(filename, []byte(content))
		logrus.Debugf("detected language %q, filename %q", language, filename)
	}

	dp, err := d.DriverPool(language)
	if err != nil {
		return language, nil, ErrUnexpected.Wrap(err)
	}

	return language, dp, nil
}

func (d *Daemon) Version(req *protocol.VersionRequest) *protocol.VersionResponse {
	return &protocol.VersionResponse{Version: d.version}
}

func (d *Daemon) Stop() error {
	var err error
	for _, dp := range d.pool {
		if cerr := dp.Stop(); cerr != nil && err != nil {
			err = cerr
		}
	}

	return err
}

// returns the default image reference for a driver given a language.
func (s *Daemon) defaultDriverImageReference(lang string) string {
	if override := s.Overrides[lang]; override != "" {
		return override
	}

	transport := s.Transport
	if transport == "" {
		transport = defaultTransport
	}

	ref := fmt.Sprintf("bblfsh/%s-driver:latest", lang)
	switch transport {
	case "docker":
		ref = "//" + ref
	}

	return fmt.Sprintf("%s:%s", transport, ref)
}

func getDriverInstanceOptions() *Options {
	l := logrus.StandardLogger()

	opts := &Options{}
	opts.LogLevel = l.Level.String()
	opts.LogFormat = "text"

	if _, ok := l.Formatter.(*logrus.JSONFormatter); ok {
		opts.LogFormat = "json"
	}

	return opts
}

func newResponseFromError(err error) protocol.Response {
	return protocol.Response{
		Status: protocol.Fatal,
		Errors: []string{err.Error()},
	}
}
