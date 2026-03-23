/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetCondition_NewCondition(t *testing.T) {
	conditions := []metav1.Condition{}

	setCondition(&conditions, "Ready", metav1.ConditionTrue, "TestReason", "Test message")

	assert.Len(t, conditions, 1)
	assert.Equal(t, "Ready", conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, conditions[0].Status)
	assert.Equal(t, "TestReason", conditions[0].Reason)
	assert.Equal(t, "Test message", conditions[0].Message)
}

func TestSetCondition_UpdateExisting(t *testing.T) {
	conditions := []metav1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "OldReason",
			Message:            "Old message",
			LastTransitionTime: metav1.Now(),
		},
	}

	setCondition(&conditions, "Ready", metav1.ConditionTrue, "NewReason", "New message")

	assert.Len(t, conditions, 1)
	assert.Equal(t, "Ready", conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, conditions[0].Status)
	assert.Equal(t, "NewReason", conditions[0].Reason)
	assert.Equal(t, "New message", conditions[0].Message)
}

func TestSetCondition_NoChangeWhenSame(t *testing.T) {
	originalTime := metav1.Now()
	conditions := []metav1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "SameReason",
			Message:            "Same message",
			LastTransitionTime: originalTime,
		},
	}

	setCondition(&conditions, "Ready", metav1.ConditionTrue, "SameReason", "Same message")

	assert.Len(t, conditions, 1)
	assert.Equal(t, originalTime, conditions[0].LastTransitionTime)
}
