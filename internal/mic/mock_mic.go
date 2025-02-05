// Code generated by MockGen. DO NOT EDIT.
// Source: mic.go
//
// Generated by this command:
//
//	mockgen -source=mic.go -package=mic -destination=mock_mic.go
//
// Package mic is a generated GoMock package.
package mic

import (
	context "context"
	reflect "reflect"

	v1beta1 "github.com/kubernetes-sigs/kernel-module-management/api/v1beta1"
	gomock "go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	v10 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MockMIC is a mock of MIC interface.
type MockMIC struct {
	ctrl     *gomock.Controller
	recorder *MockMICMockRecorder
}

// MockMICMockRecorder is the mock recorder for MockMIC.
type MockMICMockRecorder struct {
	mock *MockMIC
}

// NewMockMIC creates a new mock instance.
func NewMockMIC(ctrl *gomock.Controller) *MockMIC {
	mock := &MockMIC{ctrl: ctrl}
	mock.recorder = &MockMICMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockMIC) EXPECT() *MockMICMockRecorder {
	return m.recorder
}

// ApplyMIC mocks base method.
func (m *MockMIC) ApplyMIC(ctx context.Context, name, ns string, images []v1beta1.ModuleImageSpec, imageRepoSecret *v1.LocalObjectReference, owner v10.Object) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ApplyMIC", ctx, name, ns, images, imageRepoSecret, owner)
	ret0, _ := ret[0].(error)
	return ret0
}

// ApplyMIC indicates an expected call of ApplyMIC.
func (mr *MockMICMockRecorder) ApplyMIC(ctx, name, ns, images, imageRepoSecret, owner any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ApplyMIC", reflect.TypeOf((*MockMIC)(nil).ApplyMIC), ctx, name, ns, images, imageRepoSecret, owner)
}

// GetImageState mocks base method.
func (m *MockMIC) GetImageState(micObj *v1beta1.ModuleImagesConfig, image string) v1beta1.ImageState {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetImageState", micObj, image)
	ret0, _ := ret[0].(v1beta1.ImageState)
	return ret0
}

// GetImageState indicates an expected call of GetImageState.
func (mr *MockMICMockRecorder) GetImageState(micObj, image any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetImageState", reflect.TypeOf((*MockMIC)(nil).GetImageState), micObj, image)
}

// GetModuleImageSpec mocks base method.
func (m *MockMIC) GetModuleImageSpec(micObj *v1beta1.ModuleImagesConfig, image string) *v1beta1.ModuleImageSpec {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetModuleImageSpec", micObj, image)
	ret0, _ := ret[0].(*v1beta1.ModuleImageSpec)
	return ret0
}

// GetModuleImageSpec indicates an expected call of GetModuleImageSpec.
func (mr *MockMICMockRecorder) GetModuleImageSpec(micObj, image any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetModuleImageSpec", reflect.TypeOf((*MockMIC)(nil).GetModuleImageSpec), micObj, image)
}

// SetImageStatus mocks base method.
func (m *MockMIC) SetImageStatus(micObj *v1beta1.ModuleImagesConfig, image string, status v1beta1.ImageState) {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "SetImageStatus", micObj, image, status)
}

// SetImageStatus indicates an expected call of SetImageStatus.
func (mr *MockMICMockRecorder) SetImageStatus(micObj, image, status any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "SetImageStatus", reflect.TypeOf((*MockMIC)(nil).SetImageStatus), micObj, image, status)
}
