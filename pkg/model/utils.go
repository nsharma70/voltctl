/*
 * Copyright 2019-present Ciena Corporation
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package model

import (
	"github.com/jhump/protoreflect/dynamic"
	"log"
	"os"
)

var error = log.New(os.Stderr, "ERROR: ", 0)

func GetEnumValue(val *dynamic.Message, name string) string {
	fd := val.FindFieldDescriptorByName(name)
	if fd == nil {
		error.Fatalf("fieldDescriptor is nil for : %s", name)
	}

	enumType := fd.GetEnumType()
	if enumType == nil {
		error.Fatalf("enumType is nil for : %s", name)
	}

	field, ok := val.GetFieldByName(name).(int32)
	if !ok {
		error.Fatalf("field not found for : %s", name)
	}

	eValue := enumType.FindValueByNumber(field)
	if eValue == nil {
		error.Fatalf("eValue is nil for : %s", name)
	}

	return eValue.GetName()
}

func SetEnumValue(msg *dynamic.Message, name string, value string) {
	fd := msg.FindFieldDescriptorByName(name)
	if fd == nil {
		error.Fatalf("fieldDescriptor is nil for : %s", name)
	}

	enumType := fd.GetEnumType()
	if enumType == nil {
		error.Fatalf("enumType is nil for : %s", name)
	}

	eValue := enumType.FindValueByName(value)
	if eValue == nil {
		error.Fatalf("eValue is nil for : %s", value)
	}

	msg.SetFieldByName(name, eValue.GetNumber())
}

func GetEnumString(msg *dynamic.Message, name string, value int32) string {
	fd := msg.FindFieldDescriptorByName(name)
	if fd == nil {
		error.Fatalf("fieldDescriptor is nil for : %s", name)
	}

	enumType := fd.GetEnumType()
	if enumType == nil {
		error.Fatalf("enumType is nil for : %s", name)
	}

	eValue := enumType.FindValueByNumber(value)
	if eValue == nil {
		error.Fatalf("eValue is nil for : %s", value)
	}

	return eValue.GetName()
}
