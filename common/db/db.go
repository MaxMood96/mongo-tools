// Copyright (C) MongoDB, Inc. 2014-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

// Package db implements generic connection to MongoDB, and contains
// subpackages for specific methods of connection.
package db

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/mongodb/mongo-tools/common/log"
	"github.com/mongodb/mongo-tools/common/options"
	"github.com/youmark/pkcs8"
	"go.mongodb.org/mongo-driver/mongo"
	mopt "go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
	"go.mongodb.org/mongo-driver/tag"
)

type (
	sessionFlag uint32
)

// Session flags.
const (
	None      sessionFlag = 0
	Monotonic sessionFlag = 1 << iota
	DisableSocketTimeout
)

// MongoDB enforced limits.
const (
	MaxBSONSize = 16 * 1024 * 1024 // 16MB - maximum BSON document size
)

// Default port for integration tests.
const (
	DefaultTestPort = "33333"
)

const (
	// ignorable errors.
	ErrDuplicateKeyCode         = 11000
	ErrFailedDocumentValidation = 121
	ErrUnacknowledgedWrite      = "unacknowledged write"

	// ErrCannotInsertTimeseriesBucketsWithMixedSchema can be handled by turning TimeseriesBucketsWithMixedSchema off.
	ErrCannotInsertTimeseriesBucketsWithMixedSchema = 408
)

var ignorableWriteErrorCodes = mapset.NewSet(
	ErrDuplicateKeyCode,
	ErrFailedDocumentValidation,
)

const (
	continueThroughErrorFormat = "continuing through error: %v"
)

// Used to manage database sessions.
type SessionProvider struct {
	sync.Mutex

	// the master client used for operations
	client *mongo.Client
}

// Returns a mongo.Client connected to the database server for which the
// session provider is configured.
func (sp *SessionProvider) GetSession() (*mongo.Client, error) {
	sp.Lock()
	defer sp.Unlock()

	if sp.client == nil {
		return nil, errors.New("SessionProvider already closed")
	}

	return sp.client, nil
}

// Close closes the master session in the connection pool.
func (sp *SessionProvider) Close() {
	sp.Lock()
	defer sp.Unlock()
	if sp.client != nil {
		_ = sp.client.Disconnect(context.Background())
		sp.client = nil
	}
}

// DB provides a database with the default read preference.
func (sp *SessionProvider) DB(name string) *mongo.Database {
	return sp.client.Database(name)
}

// NewSessionProvider constructs a session provider, including a connected client.
func NewSessionProvider(opts options.ToolOptions) (*SessionProvider, error) {
	client, err := configureClient(opts)
	if err != nil {
		return nil, fmt.Errorf("error configuring the connector: %v", err)
	}
	err = client.Connect(context.Background())
	if err != nil {
		return nil, err
	}
	err = client.Ping(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %v", opts.URI.ParsedConnString(), err)
	}

	// create the provider
	return &SessionProvider{client: client}, nil
}

// addClientCertFromFile adds a client certificate to the configuration given a path to the
// containing file and returns the certificate's subject name.
func addClientCertFromFile(cfg *tls.Config, clientFile, keyPassword string) (string, error) {
	data, err := os.ReadFile(clientFile)
	if err != nil {
		return "", err
	}

	return addClientCertFromBytes(cfg, data, keyPassword)
}

func addClientCertFromSeparateFiles(
	cfg *tls.Config,
	keyFile, certFile, keyPassword string,
) (string, error) {
	keyData, err := os.ReadFile(keyFile)
	if err != nil {
		return "", err
	}
	certData, err := os.ReadFile(certFile)
	if err != nil {
		return "", err
	}

	data := append(keyData, '\n')
	data = append(data, certData...)
	return addClientCertFromBytes(cfg, data, keyPassword)
}

// addClientCertFromBytes adds a client certificate to the configuration given a path to the
// containing file and returns the certificate's subject name.
func addClientCertFromBytes(cfg *tls.Config, data []byte, keyPasswd string) (string, error) {
	var currentBlock *pem.Block
	var certDecodedBlock []byte
	var certBlocks, keyBlocks [][]byte

	remaining := data
	start := 0
	for {
		currentBlock, remaining = pem.Decode(remaining)
		if currentBlock == nil {
			break
		}

		if currentBlock.Type == "CERTIFICATE" {
			certBlock := data[start : len(data)-len(remaining)]
			certBlocks = append(certBlocks, certBlock)
			start += len(certBlock)

			// Use the first cert block for the returned Subject string at the end.
			if len(certDecodedBlock) == 0 {
				certDecodedBlock = currentBlock.Bytes
			}
		} else if strings.HasSuffix(currentBlock.Type, "PRIVATE KEY") {
			//nolint:staticcheck
			isEncrypted := x509.IsEncryptedPEMBlock(currentBlock) || strings.Contains(currentBlock.Type, "ENCRYPTED PRIVATE KEY")
			if isEncrypted {
				if keyPasswd == "" {
					return "", fmt.Errorf("no password provided to decrypt private key")
				}

				var keyBytes []byte
				var err error
				// Process the X.509-encrypted or PKCS-encrypted PEM block.
				//
				//nolint:staticcheck
				if x509.IsEncryptedPEMBlock(currentBlock) {
					// Only covers encrypted PEM data with a DEK-Info header.
					keyBytes, err = x509.DecryptPEMBlock(currentBlock, []byte(keyPasswd))
					if err != nil {
						return "", err
					}
				} else if strings.Contains(currentBlock.Type, "ENCRYPTED") {
					// The pkcs8 package only handles the PKCS #5 v2.0 scheme.
					decrypted, err := pkcs8.ParsePKCS8PrivateKey(currentBlock.Bytes, []byte(keyPasswd))
					if err != nil {
						return "", err
					}
					keyBytes, err = x509.MarshalPKCS8PrivateKey(decrypted)
					if err != nil {
						return "", err
					}
				}

				var encoded bytes.Buffer
				if err := pem.Encode(&encoded, &pem.Block{Type: currentBlock.Type, Bytes: keyBytes}); err != nil {
					return "", err
				}
				keyBlock := encoded.Bytes()
				keyBlocks = append(keyBlocks, keyBlock)
				start = len(data) - len(remaining)
			} else {
				keyBlock := data[start : len(data)-len(remaining)]
				keyBlocks = append(keyBlocks, keyBlock)
				start += len(keyBlock)
			}
		}
	}

	if len(certBlocks) == 0 {
		return "", fmt.Errorf("failed to find CERTIFICATE")
	}
	if len(keyBlocks) == 0 {
		return "", fmt.Errorf("failed to find PRIVATE KEY")
	}

	cert, err := tls.X509KeyPair(
		bytes.Join(certBlocks, []byte("\n")),
		bytes.Join(keyBlocks, []byte("\n")),
	)
	if err != nil {
		return "", err
	}

	cfg.Certificates = append(cfg.Certificates, cert)

	// The documentation for the tls.X509KeyPair indicates that the Leaf certificate is not
	// retained.
	crt, err := x509.ParseCertificate(certDecodedBlock)
	if err != nil {
		return "", err
	}

	return crt.Subject.String(), nil
}

// create a username for x509 authentication from an x509 certificate subject.
func extractX509UsernameFromSubject(subject string) string {
	// the Go x509 package gives the subject with the pairs in the reverse order from what we want.
	pairs := strings.Split(subject, ",")
	for left, right := 0, len(pairs)-1; left < right; left, right = left+1, right-1 {
		pairs[left], pairs[right] = pairs[right], pairs[left]
	}

	return strings.Join(pairs, ",")
}

// addCACertsFromFile adds root CA certificate and all the intermediate certificates in the same file to the configuration given a path
// to the containing file.
func addCACertsFromFile(cfg *tls.Config, file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}

	if cfg.RootCAs == nil {
		cfg.RootCAs = x509.NewCertPool()
	}

	if cfg.RootCAs.AppendCertsFromPEM(data) == false {
		return fmt.Errorf(
			"SSL trusted server certificates file does not contain any valid certificates. File: `%v`",
			file,
		)
	}
	return nil
}

// AKSCallback is a callback function that can be used to authenticate with Azure Kubernetes
// Service. See https://github.com/pmeredit/atlas-azure-fed-auth for testing, speficially the go
// test with AKS.
func AKSCallback(
	ctx context.Context,
	_ *mopt.OIDCArgs,
) (*mopt.OIDCCredential, error) {
	appID := os.Getenv("AZURE_APP_CLIENT_ID")
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	opts := policy.TokenRequestOptions{
		Scopes: []string{
			fmt.Sprintf("api://%s/.default", appID),
		},
	}
	token, err := cred.GetToken(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &mopt.OIDCCredential{
		AccessToken: token.Token,
		ExpiresAt:   &token.ExpiresOn,
	}, nil
}

// configure the client according to the options set in the uri and in the provided ToolOptions, with ToolOptions having precedence.
func configureClient(opts options.ToolOptions) (*mongo.Client, error) {
	if opts.URI == nil || opts.URI.ConnectionString == "" {
		// XXX Normal operations shouldn't ever reach here because a URI should
		// be created in options parsing, but tests still manually construct
		// options and generally don't construct a URI, so we invoke the URI
		// normalization routine here to correct for that.
		if err := opts.NormalizeOptionsAndURI(); err != nil {
			return nil, err
		}
	}

	clientopt := mopt.Client()
	cs := opts.URI.ParsedConnString()

	clientopt.Hosts = cs.Hosts

	if opts.RetryWrites != nil {
		clientopt.SetRetryWrites(*opts.RetryWrites)
	}

	clientopt.SetConnectTimeout(time.Duration(opts.Timeout) * time.Second)
	clientopt.SetSocketTimeout(time.Duration(opts.SocketTimeout) * time.Second)
	if opts.Connection.ServerSelectionTimeout > 0 {
		clientopt.SetServerSelectionTimeout(
			time.Duration(opts.Connection.ServerSelectionTimeout) * time.Second,
		)
	}
	if opts.ReplicaSetName != "" {
		clientopt.SetReplicaSet(opts.ReplicaSetName)
	}

	clientopt.SetAppName(opts.AppName)
	if opts.Direct && len(clientopt.Hosts) == 1 {
		clientopt.SetDirect(true)
		t := true
		clientopt.AuthenticateToAnything = &t
	}

	if opts.ReadPreference != nil {
		clientopt.SetReadPreference(opts.ReadPreference)
	}
	if opts.WriteConcern != nil {
		clientopt.SetWriteConcern(opts.WriteConcern)
	} else {
		// If no write concern was specified, default to majority
		clientopt.SetWriteConcern(writeconcern.Majority())
	}

	if opts.Compressors != "" && opts.Compressors != "none" {
		clientopt.SetCompressors(strings.Split(opts.Compressors, ","))
	}

	if cs.ZlibLevelSet {
		clientopt.SetZlibLevel(cs.ZlibLevel)
	}
	if cs.ZstdLevelSet {
		clientopt.SetZstdLevel(cs.ZstdLevel)
	}

	if cs.HeartbeatIntervalSet {
		clientopt.SetHeartbeatInterval(cs.HeartbeatInterval)
	}

	if cs.LocalThresholdSet {
		clientopt.SetLocalThreshold(cs.LocalThreshold)
	}

	if cs.MaxConnIdleTimeSet {
		clientopt.SetMaxConnIdleTime(cs.MaxConnIdleTime)
	}

	if cs.MaxPoolSizeSet {
		clientopt.SetMaxPoolSize(cs.MaxPoolSize)
	}

	if cs.MinPoolSizeSet {
		clientopt.SetMinPoolSize(cs.MinPoolSize)
	}

	if cs.LoadBalancedSet {
		clientopt.SetLoadBalanced(cs.LoadBalanced)
	}

	if cs.ReadConcernLevel != "" {
		rc := readconcern.New(readconcern.Level(cs.ReadConcernLevel))
		clientopt.SetReadConcern(rc)
	}

	if cs.ReadPreference != "" || len(cs.ReadPreferenceTagSets) > 0 || cs.MaxStalenessSet {
		readPrefOpts := make([]readpref.Option, 0, 1)

		tagSets := tag.NewTagSetsFromMaps(cs.ReadPreferenceTagSets)
		if len(tagSets) > 0 {
			readPrefOpts = append(readPrefOpts, readpref.WithTagSets(tagSets...))
		}

		if cs.MaxStaleness != 0 {
			readPrefOpts = append(readPrefOpts, readpref.WithMaxStaleness(cs.MaxStaleness))
		}

		mode, err := readpref.ModeFromString(cs.ReadPreference)
		if err != nil {
			return nil, err
		}

		readPref, err := readpref.New(mode, readPrefOpts...)
		if err != nil {
			return nil, err
		}

		clientopt.SetReadPreference(readPref)
	}

	if cs.RetryReadsSet {
		clientopt.SetRetryReads(cs.RetryReads)
	}

	if cs.JSet || cs.WString != "" || cs.WNumberSet || cs.WTimeoutSet {
		opts := make([]writeconcern.Option, 0, 1)

		if len(cs.WString) > 0 {
			opts = append(opts, writeconcern.WTagSet(cs.WString))
		} else if cs.WNumberSet {
			opts = append(opts, writeconcern.W(cs.WNumber))
		}

		if cs.JSet {
			opts = append(opts, writeconcern.J(cs.J))
		}

		if cs.WTimeoutSet {
			opts = append(opts, writeconcern.WTimeout(cs.WTimeout))
		}

		clientopt.SetWriteConcern(writeconcern.New(opts...))
	}

	if opts.Auth != nil && opts.Auth.IsSet() {
		cred := mopt.Credential{
			Username:      opts.Auth.Username,
			Password:      opts.Auth.Password,
			AuthSource:    opts.GetAuthenticationDatabase(),
			AuthMechanism: opts.Auth.Mechanism,
		}
		if cs.AuthMechanism == "MONGODB-AWS" {
			cred.Username = cs.Username
			cred.Password = cs.Password
			cred.AuthSource = cs.AuthSource
			cred.AuthMechanism = cs.AuthMechanism
			cred.AuthMechanismProperties = cs.AuthMechanismProperties
		} else if cs.AuthMechanism == "MONGODB-OIDC" {
			if env, ok := cs.AuthMechanismProperties["ENVIRONMENT"]; ok && env == "azure" {
				_, okApp := os.LookupEnv("AZURE_APP_CLIENT_ID")
				_, okClient := os.LookupEnv("AZURE_IDENTITY_CLIENT_ID")
				_, okTenant := os.LookupEnv("AZURE_TENANT_ID")
				_, okToken := os.LookupEnv("AZURE_FEDERATED_TOKEN_FILE")
				if okApp && okClient && okTenant && okToken {
					cred.OIDCMachineCallback = AKSCallback
					// We must delete the ENVIRONMENT because we are using a custom
					// callback
					delete(cs.AuthMechanismProperties, "ENVIRONMENT")
				} else if okApp || okClient || okTenant || okToken {
					return nil, fmt.Errorf(
						"must set all of AZURE_TENANT_ID, AZURE_APP_CLIENT, AZURE_IDENTITY_CLIENT_ID, " +
							"and AZURE_FEDERATED_TOKEN_FILE for Azure Kubernetes Service")
				}
			}
			cred.Username = cs.Username
			// Password is never used
			cred.AuthSource = cs.AuthSource
			cred.AuthMechanism = cs.AuthMechanism
			cred.AuthMechanismProperties = cs.AuthMechanismProperties
		}
		// Technically, an empty password is possible, but the tools don't have the
		// means to easily distinguish and so require a non-empty password.
		if cred.Password != "" {
			cred.PasswordSet = true
		}
		if opts.Kerberos != nil && cred.AuthMechanism == "GSSAPI" {
			props := make(map[string]string)
			if opts.Kerberos.Service != "" {
				props["SERVICE_NAME"] = opts.Kerberos.Service
			}
			// XXX How do we use opts.Kerberos.ServiceHost if at all?
			cred.AuthMechanismProperties = props
		}
		clientopt.SetAuth(cred)
	}

	if opts.SSL != nil && opts.UseSSL {
		// Error on unsupported features
		if opts.SSLFipsMode {
			return nil, fmt.Errorf("FIPS mode not supported")
		}
		if opts.SSLCRLFile != "" {
			return nil, fmt.Errorf("CRL files are not supported on this platform")
		}

		// #nosec G402 -- We intentionally allow known-insecure TLS options when certain CLI flags
		// are set. These are `--tlsInsecure`, `--sslAllowInvalidCertificates`, and
		// `--sslAllowInvalidHostnames`. When these are not set, we use secure TLS settings.
		tlsConfig := &tls.Config{}
		if opts.SSLAllowInvalidCert || opts.SSLAllowInvalidHost || opts.TLSInsecure {
			tlsConfig.InsecureSkipVerify = true
		}

		var x509Subject string
		keyPasswd := opts.SSL.SSLPEMKeyPassword
		var err error
		if cs.SSLClientCertificateKeyPasswordSet && cs.SSLClientCertificateKeyPassword != nil {
			keyPasswd = cs.SSLClientCertificateKeyPassword()
		}
		if cs.SSLClientCertificateKeyFileSet {
			x509Subject, err = addClientCertFromFile(
				tlsConfig,
				cs.SSLClientCertificateKeyFile,
				keyPasswd,
			)
		} else if cs.SSLCertificateFileSet || cs.SSLPrivateKeyFileSet {
			x509Subject, err = addClientCertFromSeparateFiles(tlsConfig, cs.SSLCertificateFile, cs.SSLPrivateKeyFile, keyPasswd)
		}
		if err != nil {
			return nil, fmt.Errorf(
				"error configuring client, can't load client certificate: %v",
				err,
			)
		}
		if opts.SSLCAFile != "" {
			if err := addCACertsFromFile(tlsConfig, opts.SSLCAFile); err != nil {
				return nil, fmt.Errorf("error configuring client, can't load CA file: %v", err)
			}
		}

		// If a username wasn't specified for x509, add one from the certificate.
		if clientopt.Auth != nil &&
			strings.ToLower(clientopt.Auth.AuthMechanism) == "mongodb-x509" &&
			clientopt.Auth.Username == "" {
			// The Go x509 package gives the subject with the pairs in reverse order that we want.
			clientopt.Auth.Username = extractX509UsernameFromSubject(x509Subject)
		}

		clientopt.SetTLSConfig(tlsConfig)
	}

	if cs.SSLDisableOCSPEndpointCheckSet {
		clientopt.SetDisableOCSPEndpointCheck(cs.SSLDisableOCSPEndpointCheck)
	}

	return mongo.NewClient(clientopt)
}

// FilterError determines whether an error needs to be propagated back to the user or can be continued through. If an
// error cannot be ignored, a non-nil error is returned. If an error can be continued through, it is logged and nil is
// returned.
func FilterError(stopOnError bool, err error) error {
	if err == nil || err.Error() == ErrUnacknowledgedWrite {
		return nil
	}

	if !stopOnError && CanIgnoreError(err) {
		// Just log the error but don't propagate it.
		if bwe, ok := err.(mongo.BulkWriteException); ok {
			for _, be := range bwe.WriteErrors {
				log.Logvf(log.Always, continueThroughErrorFormat, be.Message)
			}
		} else {
			log.Logvf(log.Always, continueThroughErrorFormat, err)
		}
		return nil
	}
	// Propagate this error, since it's either a fatal error or the user has turned on --stopOnError
	return err
}

// Returns whether the tools can continue when encountering the given error.
// Currently, only DuplicateKeyErrors are ignorable.
func CanIgnoreError(err error) bool {
	if err == nil {
		return true
	}

	var mongoErr mongo.ServerError
	if errors.As(err, &mongoErr) {
		for code := range ignorableWriteErrorCodes.Iter() {
			if mongoErr.HasErrorCode(code) {
				return true
			}
		}
	}

	return false
}

// Returns a boolean based on whether the given error indicates that this timeseries collection needs to be updated to set `timeseriesBucketsMayHaveMixedSchemaData` to `true`.
func TimeseriesBucketNeedsMixedSchema(err error) bool {
	var mongoErr mongo.ServerError

	return errors.As(err, &mongoErr) &&
		mongoErr.HasErrorCode(ErrCannotInsertTimeseriesBucketsWithMixedSchema)
}

// GetTimeseriesCollNameFromBucket returns a timeseries collection name from its bucket collection name.
func GetTimeseriesCollNameFromBucket(bucketCollName string) (string, error) {
	collName := strings.TrimPrefix(bucketCollName, "system.buckets.")
	if collName == bucketCollName || collName == "" {
		return "", errors.New("invalid timeseries bucket name: " + bucketCollName)
	}
	return collName, nil
}
