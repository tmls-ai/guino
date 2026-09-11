package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFullTimeGNUAndBusyBox(t *testing.T) {
	// Captured from Ubuntu GNU ls and Alpine BusyBox ls --full-time. BusyBox
	// omits fractional seconds, but both keep the timezone before the filename.
	for _, line := range []string{
		"-rw-r--r-- 1 sandbox sandbox 12 2026-09-11 14:12:24.906225003 +0000 result file.txt",
		"-rw-r--r--    1 root     root          12 2026-09-11 14:12:24 +0000 result file.txt",
	} {
		t.Run(line, func(t *testing.T) {
			output := "total 8\ndrwxrwxrwt 1 root root 4096 2026-09-11 14:12:24 +0000 .\n" +
				"drwxr-xr-x 1 root root 4096 2026-09-11 14:12:24 +0000 ..\n" + line
			files := parseLsOutput(output, "/tmp")
			require.Len(t, files, 1)
			assert.Equal(t, "result file.txt", files[0].Name)
			assert.Equal(t, "/tmp/result file.txt", files[0].Path)
			assert.EqualValues(t, 12, files[0].Size)
			assert.False(t, files[0].IsDir)
		})
	}
}
