/*
Copyright 2022 Ben Swartzlander

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

package main

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type Barb struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Gateway4 string `json:"gw4"`
	Gateway6 string `json:"gw6"`
	Cidr4    string `json:"cidr4"`
	Cidr6    string `json:"cidr6"`
}
