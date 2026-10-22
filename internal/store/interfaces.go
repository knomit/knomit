package store

//go:generate go run go.uber.org/mock/mockgen -destination=mock_batch_embedder_test.go -package=store knomit/internal/store BatchEmbedder
//go:generate go run go.uber.org/mock/mockgen -destination=../mcp/mock_batch_embedder_test.go -package=mcp knomit/internal/store BatchEmbedder
