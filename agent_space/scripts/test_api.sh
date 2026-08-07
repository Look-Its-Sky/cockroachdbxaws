API_URL="http://localhost:8080"

echo "==========================================="
echo "Testing LLM & Vector Database Integration"
echo "==========================================="

echo -e "\n1. Pinging the API to ensure it's up..."
curl -s $API_URL/ping
echo ""

echo -e "\n2. Storing some test information in the Vector Database..."
# This tests the LLM embedder and the CockroachDB insertion
curl -s -X POST $API_URL/store \
  -H "Content-Type: application/json" \
  -d '{"text": "Agent Antigravity is a highly advanced AI system designed to help developers build amazing applications quickly."}'
echo ""

echo -e "\n3. Storing a second piece of test information..."
curl -s -X POST $API_URL/store \
  -H "Content-Type: application/json" \
  -d '{"text": "CockroachDB is a cloud-native SQL database for building global, scalable cloud services that survive disasters."}'
echo ""

echo -e "\n4. Retrieving information about Agent Antigravity..."
# This tests the LLM embedder and the CockroachDB similarity search
curl -s -X POST $API_URL/retrieve \
  -H "Content-Type: application/json" \
  -d '{"query": "Who is Agent Antigravity?", "limit": 1}'
echo ""

echo -e "\n5. Retrieving information about CockroachDB..."
curl -s -X POST $API_URL/retrieve \
  -H "Content-Type: application/json" \
  -d '{"query": "What is CockroachDB?", "limit": 1}'
echo ""

echo -e "\n==========================================="
echo "Test complete!"
