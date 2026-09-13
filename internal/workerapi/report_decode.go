package workerapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

// A byte ceiling alone cannot bound []ExtractedOutput allocation: millions of
// empty JSON objects fit in a modest body. Decode the array incrementally and
// refuse its seventeenth element before allocating it.
func decodeComplete(w http.ResponseWriter, r *http.Request, value *completeRequest) bool {
	r.Body = http.MaxBytesReader(w, r.Body, executionqueue.MaximumReportBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	err := object(d, func(key string) error {
		switch key {
		case "execution_id":
			return d.Decode(&value.ExecutionID)
		case "generation":
			return d.Decode(&value.Generation)
		case "result":
			return decodeResult(d, &value.Result)
		default:
			return executionqueue.ErrInvalid
		}
	})
	if err != nil {
		return false
	}
	var extra any
	return d.Decode(&extra) == io.EOF
}

func object(d *json.Decoder, field func(string) error) error {
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return executionqueue.ErrInvalid
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return executionqueue.ErrInvalid
		}
		seen[key] = true
		if err := field(key); err != nil {
			return err
		}
	}
	token, err = d.Token()
	if err != nil || token != json.Delim('}') {
		return executionqueue.ErrInvalid
	}
	return nil
}

func decodeResult(d *json.Decoder, r *sandboxsupervisor.ExecutePythonResult) error {
	return object(d, func(key string) error {
		switch key {
		case "execution_id":
			return d.Decode(&r.ExecutionID)
		case "exit_code":
			return d.Decode(&r.ExitCode)
		case "stdout":
			return boundedText(d, &r.Stdout, 24<<20)
		case "stderr":
			return boundedText(d, &r.Stderr, 24<<20)
		case "truncated":
			return d.Decode(&r.Truncated)
		case "stdout_truncated":
			return d.Decode(&r.StdoutTruncated)
		case "stderr_truncated":
			return d.Decode(&r.StderrTruncated)
		case "terminal_reason":
			return d.Decode(&r.TerminalReason)
		case "output_error":
			return d.Decode(&r.OutputError)
		case "resource_usage":
			return d.Decode(&r.ResourceUsage)
		case "outputs":
			return decodeOutputs(d, &r.Outputs)
		default:
			return executionqueue.ErrInvalid
		}
	})
}

func boundedText(d *json.Decoder, value *string, limit int) error {
	if err := d.Decode(value); err != nil {
		return err
	}
	if len(*value) > limit {
		return executionqueue.ErrInvalid
	}
	return nil
}

func decodeOutputs(d *json.Decoder, outputs *[]sandboxsupervisor.ExtractedOutput) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return nil
	}
	if token != json.Delim('[') {
		return executionqueue.ErrInvalid
	}
	var total int64
	for d.More() {
		if len(*outputs) == 16 {
			return executionqueue.ErrInvalid
		}
		var out sandboxsupervisor.ExtractedOutput
		err := object(d, func(key string) error {
			switch key {
			case "path":
				return boundedText(d, &out.Path, 1024)
			case "size":
				return d.Decode(&out.Size)
			case "sha256":
				return boundedText(d, &out.SHA256, 64)
			case "content":
				return d.Decode(&out.Content)
			default:
				return executionqueue.ErrInvalid
			}
		})
		if err != nil {
			return err
		}
		total += int64(len(out.Content))
		if total > 32<<20 {
			return executionqueue.ErrInvalid
		}
		*outputs = append(*outputs, out)
	}
	token, err = d.Token()
	if err != nil || token != json.Delim(']') {
		return errors.New("invalid output array")
	}
	return nil
}
