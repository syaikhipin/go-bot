package agent

import (
	"testing"

	"go-harness/internal/testsupport/session"
)

func testPreparedImages(count int, bytes string) []preparedImage {
	images := make([]preparedImage, count)
	for index := range images {
		images[index] = preparedImage{attachment: session.ImageAttachment{MIMEType: "image/png"}, data: []byte(bytes)}
	}
	return images
}

func TestInlineImageBudgetPricesOnlyBase64Payload(t *testing.T) {
	// Three source bytes have four base64 bytes. The data URL header is
	// serialization framing, not part of the upstream byte budget.
	count, err := imagesToOffload(testPreparedImages(1, "abc"), imageBudget{
		maxBytes: 4, maxImages: 2, byteQuantum: 1, countQuantum: 1,
	})
	if err != nil || count != 0 {
		t.Fatalf("inline image at exact base64 limit = %d, %v", count, err)
	}
}

func TestInlineImageBudgetUsesOldestRemovalQuanta(t *testing.T) {
	images := testPreparedImages(3, "abc") // four base64 bytes each
	for _, test := range []struct {
		name   string
		budget imageBudget
		want   int
	}{
		{"count quantum", imageBudget{maxBytes: 20, maxImages: 2, byteQuantum: 1, countQuantum: 2}, 2},
		{"byte quantum", imageBudget{maxBytes: 11, maxImages: 3, byteQuantum: 4, countQuantum: 1}, 2},
		{"exact byte quantum", imageBudget{maxBytes: 11, maxImages: 3, byteQuantum: 1, countQuantum: 1}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			count, err := imagesToOffload(images, test.budget)
			if err != nil || count != test.want {
				t.Fatalf("offload count = %d, %v; want %d", count, err, test.want)
			}
		})
	}
}

func TestInlineImageBudgetRejectsQuantumAboveLimit(t *testing.T) {
	for _, budget := range []imageBudget{
		{maxBytes: 4, maxImages: 2, byteQuantum: 5, countQuantum: 1},
		{maxBytes: 4, maxImages: 2, byteQuantum: 1, countQuantum: 3},
	} {
		if _, err := imagesToOffload(nil, budget); err == nil {
			t.Fatalf("accepted invalid image budget: %+v", budget)
		}
	}
}

func TestDefaultInlineImageBudgetUsesSmallerLimitsAndQuanta(t *testing.T) {
	if defaultMaxInlineImageBytes != 15<<20 || defaultMaxImages != 30 ||
		defaultInlineByteQuantum != 5<<20 || defaultImageCountQuantum != 5 {
		t.Fatal("unexpected default inline image budget")
	}
	if count, err := imagesToOffload(testPreparedImages(30, "a"), imageBudget{}); err != nil || count != 0 {
		t.Fatalf("30 image occurrences = %d, %v", count, err)
	}
	if count, err := imagesToOffload(testPreparedImages(31, "a"), imageBudget{}); err != nil || count != 5 {
		t.Fatalf("31 image occurrences = %d, %v; want one 5-image quantum", count, err)
	}
	image := make([]byte, 1<<20)
	images := make([]preparedImage, 12)
	for index := range images {
		images[index].data = image
	}
	if count, err := imagesToOffload(images, imageBudget{}); err != nil || count != 4 {
		t.Fatalf("16 MiB encoded images = %d, %v; want 4 oldest occurrences", count, err)
	}
}
