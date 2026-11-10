#!/usr/bin/env python3
"""Reproducible local mock-upstream benchmark; no provider credentials required.
python3 scripts/benchmark.py /tmp/litegate-review --requests 200 --logs 10000,100000
Results are JSON lines. This is a short smoke load, not a capacity/SLA claim.
"""
import argparse
import concurrent.futures
import http.server
import json
import os
import secrets
import socket
import sqlite3
import subprocess
import tempfile
import threading
import time
import urllib.request
from pathlib import Path


def request(base, path, method='GET', body=None, token=None):
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    req = urllib.request.Request(base + path, data=None if body is None else json.dumps(body).encode(), headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=60) as response:
        return json.load(response)


class Upstream(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    def setup(self):
        super().setup()
        self.connection.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    def do_POST(self):
        incoming=json.loads(self.rfile.read(int(self.headers.get('Content-Length', 0))))
        if incoming.get('stream'):
            self.send_response(200)
            self.send_header('Content-Type','text/event-stream')
            self.send_header('Connection','close')
            self.end_headers()
            for _ in range(5):
                time.sleep(.01)
                self.wfile.write(b'data: {"choices":[{"delta":{"content":"x"}}]}\n\n')
                self.wfile.flush()
            self.wfile.write(b'data: {"usage":{"prompt_tokens":10,"completion_tokens":5}}\n\ndata: [DONE]\n\n')
            self.close_connection=True
            return
        time.sleep(.005)
        body = b'{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}'
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *_):
        pass


def process_stats(pid):
    raw = Path(f'/proc/{pid}/stat').read_text().split()
    return (int(raw[13]) + int(raw[14])) / os.sysconf('SC_CLK_TCK'), int(raw[23]) * os.sysconf('SC_PAGE_SIZE')


class MockServer(http.server.ThreadingHTTPServer):
    request_queue_size = 128


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('binary')
    parser.add_argument('--requests', type=int, default=200)
    parser.add_argument('--logs', default='10000,100000')
    parser.add_argument('--duration',type=float,default=0,help='seconds per scenario; 0 uses request count')
    parser.add_argument('--concurrency',default='1,10,50,100')
    parser.add_argument('--dashboard',choices=['both','on','off'],default='both')
    parser.add_argument('--stream',action='store_true')
    args = parser.parse_args()
    upstream = MockServer(('127.0.0.1', 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    try:
        for seed in map(int, args.logs.split(',')):
            with tempfile.TemporaryDirectory(prefix='litegate-bench-') as work:
                db = Path(work) / 'test.db'
                with socket.socket() as sock:
                    sock.bind(('127.0.0.1', 0))
                    port = sock.getsockname()[1]
                base = f'http://127.0.0.1:{port}'
                password = secrets.token_urlsafe(32)
                env = dict(os.environ, LITEGATE_ADMIN_PASSWORD=password)
                proc = subprocess.Popen([str(Path(args.binary).resolve()), '-addr', f'127.0.0.1:{port}', '-db', str(db)], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                try:
                    for _ in range(100):
                        try:
                            request(base, '/healthz?deep=1')
                            break
                        except OSError:
                            time.sleep(.1)
                    token = request(base, '/api/admin/login', 'POST', {'password': password})['token']
                    request(base, '/api/admin/channels', 'POST', {'name': 'mock', 'type':'openai','base_url':f'http://127.0.0.1:{upstream.server_port}/v1','api_key':'mock-only','enabled':True}, token)
                    key = request(base, '/api/admin/keys', 'POST', {'name':'benchmark'}, token)['key']
                    request(base, '/api/admin/prices', 'PUT', {'model':'m','input_price':2,'output_price':4,'currency':'USD'}, token)
                    with sqlite3.connect(db) as conn:
                        conn.executemany('INSERT INTO request_logs(model,channel_id,api_key_id,status,prompt_tokens,completion_tokens,latency_ms,cost) VALUES(?,?,?,?,?,?,?,?)', [('m',1,1,200,10,5,10,.00004)]*seed)
                    _, idle_rss = process_stats(proc.pid)
                    for concurrency in map(int,args.concurrency.split(',')):
                        for dashboard in ({'both':(False,True),'on':(True,),'off':(False,)}[args.dashboard]):
                            stop = threading.Event()
                            samples = [idle_rss]
                            wal_path=Path(str(db)+'-wal')
                            def wal_bytes():
                                try:return wal_path.stat().st_size
                                except FileNotFoundError:return 0
                            wal_start=wal_bytes()
                            wal_samples=[wal_start]
                            dashboard_errors = []
                            dashboard_times = []
                            def observe():
                                while not stop.wait(.05):
                                    try:
                                        samples.append(process_stats(proc.pid)[1])
                                        wal_samples.append(wal_bytes())
                                    except OSError:
                                        break
                            def refresh():
                                while not stop.is_set():
                                    start = time.perf_counter()
                                    try:
                                        request(base, '/api/admin/dashboard', token=token)
                                        request(base, '/api/admin/trend?dim=model&range=7d', token=token)
                                        dashboard_times.append((time.perf_counter()-start)*1000)
                                    except Exception as exc:
                                        dashboard_errors.append(type(exc).__name__)
                                    # The real UI refreshes every ten seconds.
                                    stop.wait(10)
                            sampler = threading.Thread(target=observe);sampler.start()
                            refresher = threading.Thread(target=refresh) if dashboard else None
                            if refresher: refresher.start()
                            def call(_):
                                start = time.perf_counter()
                                if args.stream:
                                    req=urllib.request.Request(base+'/v1/chat/completions',data=json.dumps({'model':'m','messages':[],'stream':True}).encode(),headers={'Authorization':'Bearer '+key,'Content-Type':'application/json'})
                                    with urllib.request.urlopen(req,timeout=60) as res:
                                        data=res.read()
                                    assert b'[DONE]' in data and b'"prompt_tokens":10' in data
                                else:
                                    result = request(base,'/v1/chat/completions','POST',{'model':'m','messages':[]},key)
                                    assert result['usage']['prompt_tokens']==10
                                return (time.perf_counter()-start)*1000
                            before_cpu,_ = process_stats(proc.pid)
                            start = time.perf_counter()
                            try:
                                with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
                                    if args.duration:
                                        deadline=time.perf_counter()+args.duration
                                        def worker(_):
                                            timings=[]
                                            while time.perf_counter()<deadline:timings.append(call(0))
                                            return timings
                                        latencies=sorted(x for batch in pool.map(worker,range(concurrency)) for x in batch)
                                    else:
                                        latencies = sorted(pool.map(call,range(args.requests)))
                                elapsed = time.perf_counter()-start
                                after_cpu,_ = process_stats(proc.pid)
                            finally:
                                stop.set();sampler.join()
                                if refresher: refresher.join()
                            with sqlite3.connect(db) as diagnostic:
                                checkpoint=diagnostic.execute('PRAGMA wal_checkpoint(PASSIVE)').fetchone()
                            def percentile(p): return round(latencies[min(len(latencies)-1, int(len(latencies)*p))],2)
                            print(json.dumps({'seed_logs':seed,'concurrency':concurrency,'dashboard':dashboard,'requests':len(latencies),'duration_s':round(elapsed,3),'stream':args.stream,'rps':round(len(latencies)/elapsed,2),'p50_ms':percentile(.5),'p95_ms':percentile(.95),'p99_ms':percentile(.99),'cpu_seconds':round(after_cpu-before_cpu,3),'peak_rss_mib':round(max(samples)/2**20,2),'idle_rss_mib':round(idle_rss/2**20,2),'rss_every_200_samples_mib':[round(v/2**20,2) for v in samples[::200]],'rss_end_mib':round(samples[-1]/2**20,2),'db_mib':round(sum(p.stat().st_size for p in Path(work).glob('test.db*'))/2**20,2),'wal_start_mib':round(wal_start/2**20,2),'wal_peak_mib':round(max(wal_samples)/2**20,2),'wal_end_mib':round(wal_bytes()/2**20,2),'passive_checkpoint':checkpoint,'dashboard_refresh_ms':dashboard_times,'dashboard_errors':dashboard_errors}),flush=True)
                finally:
                    proc.terminate()
                    try:proc.wait(timeout=5)
                    except subprocess.TimeoutExpired:proc.kill();proc.wait()
    finally:
        upstream.shutdown();upstream.server_close()

if __name__ == '__main__':
    main()
