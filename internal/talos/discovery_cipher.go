// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// The decryption scheme in this file is derived from
// github.com/siderolabs/discovery-client v0.1.15 (pkg/client/client.go,
// parseReply), Copyright Sidero Labs, Inc., licensed under the MPL-2.0. That
// code is not exported, so it is reproduced here. This file stays under the
// MPL-2.0; the rest of the package is Apache-2.0.

package talos

import (
	"crypto/cipher"
	"errors"

	clientpb "github.com/siderolabs/discovery-api/api/v1alpha1/client/pb"
)

// openAffiliate decrypts affiliate data: AES-GCM with a random nonce
// prepended to the ciphertext. gcm comes from cipher.NewGCMWithRandomNonce.
func openAffiliate(gcm cipher.AEAD, data []byte) (*clientpb.Affiliate, error) {
	plain, err := gcm.Open(nil, nil, data, nil)
	if err != nil {
		return nil, err
	}

	affiliate := &clientpb.Affiliate{}
	if err := affiliate.UnmarshalVT(plain); err != nil {
		return nil, err
	}

	return affiliate, nil
}

// openEndpoint decrypts one endpoint record: AES-ECB over
// [len][proto][zero padding], so the service can deduplicate records.
func openEndpoint(block cipher.Block, record []byte) (*clientpb.Endpoint, error) {
	bs := block.BlockSize()

	if len(record) == 0 || len(record)%bs != 0 {
		return nil, errors.New("endpoint size is not a multiple of the cipher block size")
	}

	buf := make([]byte, len(record))
	for i := 0; i < len(record); i += bs {
		block.Decrypt(buf[i:i+bs], record[i:i+bs])
	}

	size := int(buf[0])
	if size > len(buf)-1 {
		return nil, errors.New("endpoint length prefix is out of range")
	}

	endpoint := &clientpb.Endpoint{}
	if err := endpoint.UnmarshalVT(buf[1 : 1+size]); err != nil {
		return nil, err
	}

	return endpoint, nil
}
