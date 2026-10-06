package ocsp

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/platform/filesystem"
	"golang.org/x/crypto/ocsp"
)

func GetOCSPForFile(path string) ([]byte, error) {
	return filesystem.ReadFile(path)
}

func CheckOCSPFileIsNotExist(path string) bool {
	_, err := os.Stat(path)
	if err != nil {
		return os.IsNotExist(err)
	}
	return false
}

func GetOCSPStapling(cert [][]byte, path string) ([]byte, error) {
	ocspData, err := GetOCSPForFile(path)
	if err != nil {
		ocspData, err = GetOCSPForCert(cert)
		if err != nil {
			return nil, err
		}
		if !CheckOCSPFileIsNotExist(path) {
			err = os.Remove(path)
			if err != nil {
				return nil, err
			}
		}
		newFile, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		newFile.Write(ocspData)
		defer newFile.Close()
	}
	return ocspData, nil
}

func GetOCSPForCert(cert [][]byte) ([]byte, error) {
	return GetOCSPForCertContext(context.Background(), cert)
}

func GetOCSPForCertContext(ctx context.Context, cert [][]byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	bundle := new(bytes.Buffer)
	for _, derBytes := range cert {
		err := pem.Encode(bundle, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
		if err != nil {
			return nil, err
		}
	}
	pemBundle := bundle.Bytes()

	certificates, err := parsePEMBundle(pemBundle)
	if err != nil {
		return nil, err
	}
	issuedCert := certificates[0]
	if len(issuedCert.OCSPServer) == 0 {
		return nil, errors.New("no OCSP server specified in cert")
	}
	if len(certificates) == 1 {
		if len(issuedCert.IssuingCertificateURL) == 0 {
			return nil, errors.New("no issuing certificate URL")
		}
		request, errC := http.NewRequestWithContext(ctx, http.MethodGet, issuedCert.IssuingCertificateURL[0], nil)
		if errC != nil {
			return nil, errC
		}
		resp, errC := http.DefaultClient.Do(request)
		if errC != nil {
			return nil, errors.New("no issuing certificate URL")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, errors.New("issuer HTTP status: ", resp.StatusCode)
		}

		issuerBytes, errC := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if errC != nil {
			return nil, errors.New(errC)
		}

		issuerCert, errC := x509.ParseCertificate(issuerBytes)
		if errC != nil {
			return nil, errors.New(errC)
		}

		certificates = append(certificates, issuerCert)
	}
	issuerCert := certificates[1]

	ocspReq, err := ocsp.CreateRequest(issuedCert, issuerCert, nil)
	if err != nil {
		return nil, err
	}
	reader := bytes.NewReader(ocspReq)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, issuedCert.OCSPServer[0], reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/ocsp-request")
	req, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, errors.New(err)
	}
	defer req.Body.Close()
	if req.StatusCode != http.StatusOK {
		return nil, errors.New("OCSP HTTP status: ", req.StatusCode)
	}
	ocspResBytes, err := io.ReadAll(io.LimitReader(req.Body, (1<<20)+1))
	if err != nil {
		return nil, errors.New(err)
	}
	if len(ocspResBytes) > 1<<20 {
		return nil, errors.New("OCSP response exceeds 1 MiB")
	}
	response, err := ocsp.ParseResponseForCert(ocspResBytes, issuedCert, issuerCert)
	if err != nil {
		return nil, errors.New("invalid OCSP response").Base(err)
	}
	if !response.NextUpdate.IsZero() && response.NextUpdate.Before(time.Now()) {
		return nil, errors.New("expired OCSP response")
	}
	return ocspResBytes, nil
}

// parsePEMBundle parses a certificate bundle from top to bottom and returns
// a slice of x509 certificates. This function will error if no certificates are found.
func parsePEMBundle(bundle []byte) ([]*x509.Certificate, error) {
	var certificates []*x509.Certificate
	var certDERBlock *pem.Block

	for {
		certDERBlock, bundle = pem.Decode(bundle)
		if certDERBlock == nil {
			break
		}

		if certDERBlock.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(certDERBlock.Bytes)
			if err != nil {
				return nil, err
			}
			certificates = append(certificates, cert)
		}
	}

	if len(certificates) == 0 {
		return nil, errors.New("no certificates were found while parsing the bundle")
	}

	return certificates, nil
}
