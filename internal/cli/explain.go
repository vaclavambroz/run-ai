package cli

import (
	"encoding/json"
	"fmt"
	"io"
)

func WriteExplanation(writer io.Writer, request Request) error {
	request.Help = false
	request.Explain = false
	payload, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal explanation: %w", err)
	}
	if _, err := writer.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("write explanation: %w", err)
	}
	return nil
}
