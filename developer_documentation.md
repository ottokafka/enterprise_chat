


## normal chat: request
```sh
curl https://alice.forest-interactive.com/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer API_KEY" \
  -d '{
    "model": "Qwen3.5-35B-A3B-GGUF",
    "messages": [
      { "role": "system", "content": "You are a helpful assistant." },
      { "role": "user", "content": "Hello!" }
    ],
    "temperature": 0.7
  }'


```

# response 

```sh
{
  "id": "chatcmpl-123",
  "choices": [{
    "index": 0,
    "message": { "role": "assistant", "content": "Hello! How can I assist you?" },
    "finish_reason": "stop"
  }]
}

```

# Multimodal with Vision: Chat Request

```sh
curl https://alice.forest-interactive.com/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer API_KEY" \
  -d '{
    "model": "Qwen3.5-35B-A3B-GGUF",
    "messages": [
      {
        "role": "user",
        "content": [
          { "type": "text", "text": "What is in this image?" },
          { "type": "image_url", "image_url": { "url": "data:image/jpeg;base64,/9j/4AAQSkZ..." } }
        ]
      }
    ]
  }'

```

reponse

```sh
{
  "id": "chatcmpl-vision-123",
  "choices": [{
    "index": 0,
    "message": { "role": "assistant", "content": "This image shows a siamese cat sitting on a wooden floor." },
    "finish_reason": "stop"
  }]
}

```

embeddings

```sh
curl https://alice.forest-interactive.com/v1/embeddings \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer API_KEY" \
  -d '{
    "model": "Qwen3-Embedding-8B-GGUF",
    "input": "The food was delicious and the service was excellent."
  }'

```

reponse

```sh
{
  "object": "list",
  "data": [
    {
      "object": "embedding",
      "index": 0,
      "embedding": [
        -0.006929283495992422,
        -0.005336422007530928,
        ...
      ]
    }
  ],
  "model": "Qwen3-Embedding-8B-GGUF",
  "usage": {
    "prompt_tokens": 5,
    "total_tokens": 5
  }
}

```

# image generation 

```sh
curl https://alice.forest-interactive.com/v1/images/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer API_KEY" \
  -d '{
    "prompt": "a white siamese cat",
    "size": "512x512",
    "response_format": "b64_json"
  }'

```
 reponse
```sh
{
  "created": 1728576000,
  "data": [ { "b64_json": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==" } ]
}

```

Rerank request

```sh

curl -X POST http://192.168.1.235:8082/v1/rerank \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen3-Reranker-8B",
    "query": "What is machine learning?",
    "documents": [
      "Machine learning is a subset of artificial intelligence that enables systems to learn from data.",
      "Deep learning uses neural networks with multiple layers to process information.",
      "Artificial intelligence encompasses both machine learning and deep learning."
    ],
    "top_n": 2
  }'

```
reponse
```sh
{"model":"Qwen3-Reranker-8B","object":"list","usage":{"prompt_tokens":266,"total_tokens":266},"results":[{"index":0,"relevance_score":0.9836259484291077},{"index":2,"relevance_score":0.10805725306272507}]}
```