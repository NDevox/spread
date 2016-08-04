package data

import (
	"encoding/json"
	"errors"
	"fmt"

	pb "rsprd.com/spread/pkg/spreadproto"
)

// CreateObject uses reflection to convert the data (usually a struct) into an Object.
func CreateObject(key string, ptr interface{}) (*pb.Object, error) {
	data, err := json.Marshal(ptr)
	if err != nil {
		return nil, fmt.Errorf("unable to generate JSON object: %v", err)
	}

	// TODO: use reflection to replace this
	var jsonData map[string]interface{}
	err = json.Unmarshal(data, &jsonData)
	if err != nil {
		return nil, err
	}

	return ObjectFromMap(key, jsonData)
}

// ObjectFromMap creates an Object, using the entries of a map as fields.
// This supports maps embedded as values. It is assumed that types are limited to JSON types.
func ObjectFromMap(key string, data map[string]interface{}) (*pb.Object, error) {
	items := make(map[string]*pb.Field, len(data))
	for k, v := range data {
		field, err := buildField(k, v)
		if err != nil {
			return nil, err
		}
		items[k] = field
	}
	return &pb.Object{
		Items: items,
	}, nil
}

func MapFromObject(obj *pb.Object) (map[string]interface{}, error) {
	items := obj.GetItems()
	if items == nil {
		return nil, ErrObjectNilFields
	}

	out := make(map[string]interface{}, len(items))
	for _, field := range items {
		val, err := decodeField(field)
		if err != nil {
			return nil, fmt.Errorf("could not decode field '%s': %v", field.Key, err)
		}
		out[field.Key] = val
	}
	return out, nil
}

var (
	ErrObjectNilFields = errors.New("object had nil for Fields")
)
