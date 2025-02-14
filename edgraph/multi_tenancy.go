//go:build oss
// +build oss

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package edgraph

import "context"

type ResetPasswordInput struct {
	UserID    string
	Password  string
	Namespace uint64
}

func (s *Server) CreateNamespaceInternal(ctx context.Context, passwd string) (uint64, error) {
	return 0, nil
}

func (s *Server) DeleteNamespace(ctx context.Context, namespace uint64) error {
	return nil
}

func (s *Server) ResetPassword(ctx context.Context, ns *ResetPasswordInput) error {
	return nil
}

func createGuardianAndGroot(ctx context.Context, namespace uint64) error {
	return nil
}
