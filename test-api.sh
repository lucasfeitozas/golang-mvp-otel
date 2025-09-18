#!/bin/bash

echo "=== Testando Sistema de Temperatura por CEP ==="
echo ""

# Verificar se os serviços estão rodando
echo "1. Verificando se os serviços estão rodando..."
if ! curl -s http://localhost:8080 > /dev/null 2>&1; then
    echo "❌ Serviço A não está rodando em http://localhost:8080"
    echo "Execute: docker-compose up -d"
    exit 1
fi

if ! curl -s http://localhost:8081 > /dev/null 2>&1; then
    echo "❌ Serviço B não está rodando em http://localhost:8081"
    echo "Execute: docker-compose up -d"
    exit 1
fi

echo "✅ Serviços estão rodando"
echo ""

# Teste 1: CEP válido
echo "2. Testando CEP válido (01001000 - São Paulo)..."
response=$(curl -s -X POST http://localhost:8080/ \
  -H "Content-Type: application/json" \
  -d '{"cep": "01001000"}')

echo "Response: $response"
echo ""

# Teste 2: CEP inválido (formato)
echo "3. Testando CEP inválido (formato)..."
response=$(curl -s -X POST http://localhost:8080/ \
  -H "Content-Type: application/json" \
  -d '{"cep": "123"}')

echo "Response: $response"
echo ""

# Teste 3: CEP inexistente
echo "4. Testando CEP inexistente..."
response=$(curl -s -X POST http://localhost:8080/ \
  -H "Content-Type: application/json" \
  -d '{"cep": "99999999"}')

echo "Response: $response"
echo ""

# Teste 4: Verificar Zipkin
echo "5. Verificando Zipkin UI..."
if curl -s http://localhost:9411 > /dev/null 2>&1; then
    echo "✅ Zipkin UI disponível em http://localhost:9411"
else
    echo "❌ Zipkin UI não está disponível"
fi

echo ""
echo "=== Testes concluídos ==="
echo "Para visualizar traces, acesse: http://localhost:9411"