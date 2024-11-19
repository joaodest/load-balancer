import requests
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from collections import Counter
import statistics

class LoadBalancerTester:
    def __init__(self, base_url='http://localhost:8080'):
        self.base_url = base_url
        self.session = requests.Session()
        self.response_times = []
        self.server_responses = []

    def make_request(self):
        try:
            start_time = time.time()
            response = self.session.get(self.base_url)
            end_time = time.time()
            
            # Captura o tempo de resposta
            response_time = (end_time - start_time) * 1000  # em millisegundos
            self.response_times.append(response_time)
            
            # Captura qual servidor respondeu baseado no conteúdo da resposta
            server = response.text.strip()  # "Resposta do servidor na porta XXXX"
            self.server_responses.append(server)
            
            return {
                'status_code': response.status_code,
                'response_time': response_time,
                'server': server
            }
        except Exception as e:
            return {'error': str(e)}

    def test_health_endpoints(self):
        print("\nTestando endpoints de health check:")
        servers = ['8001', '8002']
        for port in servers:
            try:
                response = requests.get(f'http://localhost:{port}/health')
                print(f"Servidor {port}: {response.status_code} - {response.text}")
            except Exception as e:
                print(f"Servidor {port}: Erro - {str(e)}")

    def run_load_test(self, total_requests, concurrent_requests):
        print(f"\nIniciando teste de carga com {total_requests} requisições ({concurrent_requests} concorrentes)")
        
        start_time = time.time()
        results = []
        
        with ThreadPoolExecutor(max_workers=concurrent_requests) as executor:
            futures = [executor.submit(self.make_request) for _ in range(total_requests)]
            for future in futures:
                results.append(future.result())
        
        end_time = time.time()
        duration = end_time - start_time
        
        # Análise dos resultados
        success_count = len([r for r in results if 'error' not in r])
        server_distribution = Counter(self.server_responses)
        
        # Estatísticas de tempo de resposta
        avg_response_time = statistics.mean(self.response_times)
        median_response_time = statistics.median(self.response_times)
        if len(self.response_times) > 1:
            stddev_response_time = statistics.stdev(self.response_times)
        else:
            stddev_response_time = 0
        
        # Relatório
        print(f"\nRelatório do teste de carga:")
        print(f"{'='*50}")
        print(f"Estatísticas gerais:")
        print(f"- Total de requisições: {total_requests}")
        print(f"- Requisições bem sucedidas: {success_count}")
        print(f"- Taxa de sucesso: {(success_count/total_requests)*100:.2f}%")
        print(f"- Tempo total: {duration:.2f} segundos")
        print(f"- Requisições por segundo: {total_requests/duration:.2f}")
        
        print(f"\nDistribuição entre servidores:")
        for server, count in server_distribution.items():
            print(f"- {server}: {count} ({(count/total_requests)*100:.2f}%)")
        
        print(f"\nTempos de resposta (ms):")
        print(f"- Média: {avg_response_time:.2f}")
        print(f"- Mediana: {median_response_time:.2f}")
        print(f"- Desvio padrão: {stddev_response_time:.2f}")
        print(f"- Mínimo: {min(self.response_times):.2f}")
        print(f"- Máximo: {max(self.response_times):.2f}")

    def test_sequential_requests(self, num_requests=5):
        print("\nTestando requisições sequenciais com mesma sessão:")
        session = requests.Session()
        for i in range(num_requests):
            try:
                response = session.get(self.base_url)
                print(f"Request {i+1}: {response.text.strip()}")
                if 'lb_session' in response.cookies:
                    print(f"Session cookie: {response.cookies['lb_session']}")
            except Exception as e:
                print(f"Request {i+1}: Erro - {str(e)}")
            time.sleep(0.5)  # Pequena pausa entre requisições

if __name__ == "__main__":
    tester = LoadBalancerTester()
    
    # Teste os health checks primeiro
    tester.test_health_endpoints()
    
    # Teste básico
    print("\nExecutando teste básico...")
    tester.run_load_test(total_requests=50, concurrent_requests=5)
    
    # Teste de carga mais intenso
    print("\nExecutando teste de carga intenso...")
    tester.run_load_test(total_requests=200, concurrent_requests=20)
    
    # Teste de sessão sticky
    print("\nTestando possível comportamento de sessão sticky...")
    tester.test_sequential_requests()