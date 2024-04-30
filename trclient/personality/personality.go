// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package personality runs a simple Trillian personality.
package personality

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/google/trillian"
	tt "github.com/google/trillian/types"
	"github.com/transparency-dev/formats/log"
	"github.com/transparency-dev/merkle/rfc6962"
	"golang.org/x/mod/sumdb/note"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	connectTimeout = flag.Duration("connect_timeout", 5*time.Second, "the timeout for connecting to the backend")
)

// SignedCheckpoint is a serialised form of a checkpoint+signatures.
type SignedCheckpoint []byte

// TrillianP is a personality backed by a trillian log.
type TrillianClient struct {
	LogClient trillian.TrillianLogClient
	TreeID    int64
	Counter   int
	LastTag   []byte
	Signer    note.Signer
}

// NewPersonality creates a new Trillian personality from the flags.
func NewPersonality(logAddr string, treeID int64, s note.Signer) (*TrillianClient, error) {
	if treeID <= 0 {
		return nil, fmt.Errorf("tree_id must be provided and positive, got %d", treeID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *connectTimeout)
	defer cancel()
	conn, err := grpc.DialContext(ctx, logAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, fmt.Errorf("did not connect to trillian on %v: %v", logAddr, err)
	}

	log := trillian.NewTrillianLogClient(conn)
	x := &TrillianClient{
		LogClient: log,
		TreeID:    treeID,
		Counter:   0,
		LastTag:   nil,
		Signer:    s,
	}

	return x, nil
}

// formLeaf creates a Trillian log leaf from an entry.
func (p *TrillianClient) formLeaf(entry []byte) *trillian.LogLeaf {
	leafHash := rfc6962.DefaultHasher.HashLeaf(entry)
	return &trillian.LogLeaf{
		LeafValue:      entry,
		MerkleLeafHash: leafHash,
	}
}

// getCheckpoint fetches the latest Trillian root and creates a checkpoint from it.
func (p *TrillianClient) getCheckpoint(ctx context.Context) (*log.Checkpoint, error) {
	req := trillian.GetLatestSignedLogRootRequest{LogId: p.TreeID}
	resp, err := p.LogClient.GetLatestSignedLogRoot(ctx, &req)
	if err != nil {
		return nil, err
	}
	// Unpack the response and convert it to the local Checkpoint
	// representation.
	root := resp.GetSignedLogRoot()
	var logRoot tt.LogRootV1
	if err := logRoot.UnmarshalBinary(root.LogRoot); err != nil {
		return nil, err
	}
	return &log.Checkpoint{
		Origin: "Hello World Log",
		Hash:   logRoot.RootHash,
		Size:   logRoot.TreeSize,
	}, nil
}

// GetChkpt gets the latest checkpoint.
func (p *TrillianClient) GetChkpt(ctx context.Context) (SignedCheckpoint, error) {
	cp, err := p.getCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Trillian checkpoint: %w", err)
	}
	s, err := note.Sign(&note.Note{Text: string(cp.Marshal())}, p.Signer)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Append adds an entry to the Trillian log and waits to return the new checkpoint.
func (p *TrillianClient) Append(ctx context.Context, entry []byte) (SignedCheckpoint, error) {
	// First get the latest checkpoint.
	chkpt, err := p.getCheckpoint(ctx)
	if err != nil {
		return nil, err
	}
	leaf := p.formLeaf(entry)
	req := trillian.QueueLeafRequest{LogId: p.TreeID, Leaf: leaf}
	if _, err := p.LogClient.QueueLeaf(ctx, &req); err != nil {
		return nil, err
	}
	// Now fetch the new checkpoint, keep going until it's there and
	// return an error at some point if it isn't.
	for start := time.Now(); time.Since(start) < 5*time.Second; {
		chkptNew, err := p.getCheckpoint(ctx)
		if err != nil {
			return nil, err
		}
		// TODO(meiklejohn): should probably verify that the specific entry was
		// incorporated into the tree too.
		if chkpt.Size < chkptNew.Size {
			s, err := note.Sign(&note.Note{Text: string(chkptNew.Marshal())}, p.Signer)
			if err != nil {
				return nil, err
			}
			return s, nil
		}
	}
	return nil, fmt.Errorf("did not get an updated checkpoint")
}

// ProveIncl returns an inclusion proof for a given checkpoint and entry.
func (p *TrillianClient) ProveIncl(ctx context.Context, chkptSize uint64, entry []byte) (*trillian.Proof, error) {
	// Form the leaf from the entry.
	leaf := p.formLeaf(entry)
	// Form the request according to the Trillian API.
	req := trillian.GetInclusionProofByHashRequest{
		LogId:    p.TreeID,
		LeafHash: leaf.MerkleLeafHash,
		TreeSize: int64(chkptSize),
	}
	// Process the response.
	resp, err := p.LogClient.GetInclusionProofByHash(ctx, &req)
	if err != nil {
		return nil, err
	}
	return resp.GetProof()[0], nil
}

// UpdateChkpt gets the latest checkpoint for the Trillian log and proves its
// consistency with a provided one.
func (p *TrillianClient) UpdateChkpt(ctx context.Context, chkptSize uint64) (SignedCheckpoint, *trillian.Proof, error) {
	// First get the latest checkpoint
	chkptNew, err := p.getCheckpoint(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Now get a consistency proof if one is needed.
	var pf *trillian.Proof
	if chkptNew.Size > chkptSize {
		req := trillian.GetConsistencyProofRequest{
			LogId:          p.TreeID,
			FirstTreeSize:  int64(chkptSize),
			SecondTreeSize: int64(chkptNew.Size),
		}
		resp, err := p.LogClient.GetConsistencyProof(ctx, &req)
		if err != nil {
			return nil, nil, err
		}
		pf = resp.GetProof()
	}
	s, err := note.Sign(&note.Note{Text: string(chkptNew.Marshal())}, p.Signer)
	if err != nil {
		return nil, nil, err
	}
	return s, pf, nil
}
