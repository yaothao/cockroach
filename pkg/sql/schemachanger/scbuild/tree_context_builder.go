// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package scbuild

import (
	"github.com/cockroachdb/cockroach/pkg/sql/faketreeeval"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scbuild/internal/scbuildstmt"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
)

var _ scbuildstmt.TreeContextBuilder = buildCtx{}

// SemaCtx implements the scbuildstmt.TreeContextBuilder interface.
func (b buildCtx) SemaCtx() *tree.SemaContext {
	return b.Dependencies.SemaCtx()
}

// EvalCtx implements the scbuildstmt.TreeContextBuilder interface.
func (b buildCtx) EvalCtx() *eval.Context {
	return newEvalCtx(b.Dependencies)
}

func newEvalCtx(d Dependencies) *eval.Context {
	return &eval.Context{
		ClusterID:            d.ClusterID(),
		SessionDataStack:     sessiondata.NewStack(d.SessionData()),
		Planner:              &faketreeeval.DummyEvalPlanner{},
		StreamManagerFactory: &faketreeeval.DummyStreamManagerFactory{},
		PrivilegedAccessor:   &faketreeeval.DummyPrivilegedAccessor{},
		SessionAccessor:      &faketreeeval.DummySessionAccessor{},
		ClientNoticeSender:   d.ClientNoticeSender(),
		Sequence:             &faketreeeval.DummySequenceOperators{},
		Tenant:               &faketreeeval.DummyTenantOperator{},
		Regions:              &faketreeeval.DummyRegionOperator{},
		Settings:             d.ClusterSettings(),
		Codec:                d.Codec(),
		DescIDGenerator:      d.DescIDGenerator(),
		Placeholders:         &d.SemaCtx().Placeholders,
	}
}
