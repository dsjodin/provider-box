package msca

import "encoding/asn1"

// A certs-only PKCS#7 SignedData (RFC 2315) - a "certificate bag" with no
// signers - is what ADCS returns from certnew.p7b. The Go standard library has
// no PKCS#7 encoder, so build the minimal degenerate structure by hand rather
// than pull in a dependency (the module only depends on pgx today).

var (
	oidSignedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidData       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
)

type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"explicit,tag:0"`
}

type innerContent struct {
	ContentType asn1.ObjectIdentifier
}

type signedData struct {
	Version          int
	DigestAlgorithms []asn1.RawValue `asn1:"set"`
	ContentInfo      innerContent
	Certificates     asn1.RawValue // [0] IMPLICIT SET OF certificate
	SignerInfos      []asn1.RawValue `asn1:"set"`
}

// degenerateP7B wraps the given DER certificates in a signer-less PKCS#7
// SignedData and returns its DER encoding.
func degenerateP7B(certsDER [][]byte) ([]byte, error) {
	var certBytes []byte
	for _, c := range certsDER {
		certBytes = append(certBytes, c...)
	}
	sd := signedData{
		Version:          1,
		DigestAlgorithms: []asn1.RawValue{},
		ContentInfo:      innerContent{ContentType: oidData},
		Certificates:     asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: certBytes},
		SignerInfos:      []asn1.RawValue{},
	}
	sdDER, err := asn1.Marshal(sd)
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(contentInfo{
		ContentType: oidSignedData,
		Content:     asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: sdDER},
	})
}
