// Copyright 2017 Google LLC. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package errors

import (
	"database/sql"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	_ "k8s.io/klog/v2"
)

func TestWrapError(t *testing.T) {
	grpcErr := status.Errorf(codes.NotFound, "not found err")
	err := errors.New("generic error")

	tests := []struct {
		err     error
		wantErr error
	}{
		{
			err:     grpcErr,
			wantErr: grpcErr,
		},
		{
			err:     err,
			wantErr: err,
		},
		{
			err:     sql.ErrNoRows,
			wantErr: status.Error(codes.NotFound, sql.ErrNoRows.Error()),
		},
	}
	for _, test := range tests {
		// We can't use == for rpcErrors because grpc.Errorf returns *rpcError.
		if gotErr := WrapError(test.err); gotErr.Error() != test.wantErr.Error() {
			t.Errorf("WrapError('%T') = %v, want %v", test.err, gotErr, test.wantErr)
		}
	}
}
