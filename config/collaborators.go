package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"github.com/golang/glog"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This exists to consolidate construction of clients to collaborating
// systems or other shared resources. We may need to do some poor man's
// DI here with build flags and selective compilation against varying
// concrete interfaces here.

type Collaborators struct {
	HTTPClientFactory   *HTTPClientFactory
	KeyFileNamesFetcher *KeyFileNamesFetcher
}

func NewCollaborators(hConfig HorizonConfig) (*Collaborators, error) {
	httpClientFactory, err := newHTTPClientFactory(hConfig)
	if err != nil {
		return nil, err
	}

	keyFileNameFetcher, err := newKeyFileNamesFetcher(hConfig)
	if err != nil {
		return nil, err
	}

	return &Collaborators{
		HTTPClientFactory:   httpClientFactory,
		KeyFileNamesFetcher: keyFileNameFetcher,
	}, nil
}

func (c *Collaborators) String() string {
	return fmt.Sprintf("HTTPClientFactory: %v, KeyFileNamesFetcher: %v", c.HTTPClientFactory, c.KeyFileNamesFetcher)
}

type HTTPClientFactory struct {
	NewHTTPClient func(overrideTimeoutS *uint) *http.Client
	RetryCount    int // number of retries for tranport error.
	RetryInterval int // retry interval in second for tranport error. The default is 10 seconds.
}

// default retry interval is 10 seconds
func (h *HTTPClientFactory) GetRetryInterval() int {
	if h.RetryInterval == 0 {
		return 10
	} else {
		return h.RetryInterval
	}
}

type KeyFileNamesFetcher struct {
	// get all the pem file names from the pulic key path and user key path.
	// if the publicKeyPath is a file name all the *.pem files within the same directory will be returned.
	// userkeyPath is always a directory.
	GetKeyFileNames func(publicKeyPath, userKeyPath string) ([]string, error)
}

// WrappedHTTPClient is a function producer that wraps an HTTPClient's
// NewHTTPClient method in a generic function call for compatibilty with
// external callers.
func (f *HTTPClientFactory) WrappedNewHTTPClient() func(*uint) *http.Client {
	return func(overrideTimeoutS *uint) *http.Client {
		return f.NewHTTPClient(overrideTimeoutS)
	}
}

// TODO: use a pool of clients instead of creating them forever
func newHTTPClientFactory(hConfig HorizonConfig) (*HTTPClientFactory, error) {
	var derCerts, pemCerts []byte
	var cerCerts [][]byte
	var mgmtHubBytes []byte
	var cssCaBytes []byte

	if hConfig.Edge.CACertsPath != "" {
		var err error
		err = filepath.Walk(hConfig.Edge.CACertsPath,
			func(path string, f os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if !f.IsDir() {
					ext := filepath.Ext(path)
					ok := func(ext string) bool {
						extensions := []string{".der", ".pem", ".crt", ".cer"}
						for _, extension := range extensions {
							if ext == extension {
								return true
							}
						}
						return false
					}(ext)
					if ok {
						caBytes, err := ioutil.ReadFile(path)
						if err != nil {
							return fmt.Errorf("Failed to read CACertsFile: %v", path)
						}
						switch ext {
						case ".der":
							derCerts = append(derCerts, caBytes...)
						case ".pem":
							pemCerts = append(pemCerts, caBytes...)
						case ".cer", ".crt":
							cerCerts = append(cerCerts, caBytes)

						}
						glog.V(4).Infof("Read CA certs from provided file %v", path)
					}
				}
				return nil
			})
		if err != nil {
			return nil, fmt.Errorf("Failed to read CACertsFiles from: %v", hConfig.Edge.CACertsPath)
		}
	}

	// A custom TLS certificate can be set in the /var/default/horizon file. Anax sees this value as
	// an environment variable when it is started. If the Horizon management hub (Exchange, CSS, etc) is
	// using a self signed cert or a cert from an unknown authority, it can be set here so that anax
	// will use it. This means the node owner doesnt have to add the cert to the trust store of the node's
	// operating system.
	mhCertPath := os.Getenv(OldMgmtHubCertPath)
	if mhCertPath == "" {
		mhCertPath = os.Getenv(ManagementHubCertPath)
	}

	if mhCertPath != "" {
		var err error
		mgmtHubBytes, err = ioutil.ReadFile(mhCertPath)
		if err != nil {
			return nil, fmt.Errorf("Failed to read Cert File: %v", mhCertPath)
		}
		glog.V(4).Infof("Read Management Hub cert from provided file %v", mhCertPath)
	}

	if hConfig.AgreementBot.CSSSSLCert != "" {
		var err error
		cssCaBytes, err = ioutil.ReadFile(hConfig.AgreementBot.CSSSSLCert)
		if err != nil {
			return nil, fmt.Errorf("Failed to read Agbot CSS SSL Cert File: %v", hConfig.AgreementBot.CSSSSLCert)
		}
		glog.V(4).Infof("Read CSS cert from provided file %v", hConfig.AgreementBot.CSSSSLCert)
	}

	var tlsConf tls.Config
	tlsConf.InsecureSkipVerify = false
	// do not allow negotiation to previous versions of TLS
	tlsConf.MinVersion = tls.VersionTLS12

	var certPool *x509.CertPool

	if hConfig.Edge.TrustSystemCACerts || hConfig.AgreementBot.CSSSSLCert != "" {
		var err error
		certPool, err = x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		glog.V(4).Info("Added distribution-provided CA Certs to trust")

	} else {
		certPool = x509.NewCertPool()
	}

	if len(pemCerts) != 0 {
		certPool.AppendCertsFromPEM(pemCerts)
	}
	if len(derCerts) != 0 {
		derCerts, _ := x509.ParseCertificates(derCerts)
		if derCerts != nil {
			for _, derCert := range derCerts {
				certPool.AddCert(derCert)
			}
		}
	}
	if len(cerCerts) != 0 {
		for _, caBytes := range cerCerts {
			if len(caBytes) != 0 {
				if ok := certPool.AppendCertsFromPEM(caBytes); !ok {
					cerCert, _ := x509.ParseCertificate(caBytes)
					if cerCert != nil {
						certPool.AddCert(cerCert)
					}
				}
			}
		}
	}
	if len(mgmtHubBytes) != 0 {
		certPool.AppendCertsFromPEM(mgmtHubBytes)
	}
	if len(cssCaBytes) != 0 {
		certPool.AppendCertsFromPEM(cssCaBytes)
	}

	tlsConf.RootCAs = certPool

	tlsConf.BuildNameToCertificate()

	clientFunc := func(overrideTimeoutS *uint) *http.Client {
		var timeoutS uint

		if overrideTimeoutS != nil {
			timeoutS = *overrideTimeoutS
		} else {
			timeoutS = hConfig.Edge.DefaultHTTPClientTimeoutS
		}

		return &http.Client{
			// remember that this timouet is for the whole request, including
			// body reading. This means that you must set the timeout according
			// to the total payload size you expect
			Timeout: time.Second * time.Duration(timeoutS),
			Transport: &http.Transport{
				Dial: (&net.Dialer{
					Timeout:   20 * time.Second,
					KeepAlive: 60 * time.Second,
				}).Dial,
				TLSHandshakeTimeout:   20 * time.Second,
				ResponseHeaderTimeout: 20 * time.Second,
				ExpectContinueTimeout: 8 * time.Second,
				MaxIdleConns:          MaxHTTPIdleConnections,
				IdleConnTimeout:       HTTPIdleConnectionTimeoutS * time.Second,
				TLSClientConfig:       &tlsConf,
			},
		}
	}

	return &HTTPClientFactory{
		NewHTTPClient: clientFunc,
		RetryCount:    0,
		RetryInterval: 10,
	}, nil
}

func newKeyFileNamesFetcher(hConfig HorizonConfig) (*KeyFileNamesFetcher, error) {

	// get all the *.pem files under the given directory
	getPemFiles := func(homePath string) ([]string, error) {
		pemFileNames := make([]string, 0, 10)

		if files, err := ioutil.ReadDir(homePath); err != nil && !os.IsNotExist(err) {
			return nil, errors.New(fmt.Sprintf("Unable to get list of PEM files in %v, error: %v", homePath, err))
		} else if os.IsNotExist(err) {
			return pemFileNames, nil
		} else {
			for _, fileInfo := range files {
				if strings.HasSuffix(fileInfo.Name(), ".pem") && !fileInfo.IsDir() {
					pemFileNames = append(pemFileNames, fmt.Sprintf("%v/%v", homePath, fileInfo.Name()))
				}
			}
			return pemFileNames, nil
		}
	}

	// get the pem file names from the pulic key path and user key path.
	// if the publicKeyPath is a file name all the *.pem files within the same directory will be returned.
	// userkeyPath is always a directory.
	getKeyFilesFunc := func(publicKeyPath, userKeyPath string) ([]string, error) {
		keyFileNames := make([]string, 0)

		// only check these keys too if publicKeyPath was specified (this is behavior to accomodate legacy config)
		if publicKeyPath != "" {
			// Compute the public key directory based on the configured platform public key file location.
			pubKeyDir := publicKeyPath[:strings.LastIndex(publicKeyPath, "/")]

			// Grab all PEM files from that location and try to verify the signature against each one.
			if pemFiles, err := getPemFiles(pubKeyDir); err != nil {
				return keyFileNames, err
			} else {
				keyFileNames = append(keyFileNames, pemFiles...)
			}
		}

		// Grab all PEM files from userKeyPath
		if userKeyPath != "" {
			if pemFiles, err := getPemFiles(userKeyPath); err != nil {
				return keyFileNames, err
			} else {
				keyFileNames = append(keyFileNames, pemFiles...)
			}
		}
		return keyFileNames, nil
	}

	return &KeyFileNamesFetcher{
		GetKeyFileNames: getKeyFilesFunc,
	}, nil
}
