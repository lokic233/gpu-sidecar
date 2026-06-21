// Portable controlled GPU workload (AMD/HIP build). Mirror of workload.cu.
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <csignal>
#include <unistd.h>
#include <hip/hip_runtime.h>

volatile sig_atomic_t stop_flag = 0;
void on_sig(int){ stop_flag = 1; }

__global__ void burn(float* a, int n, int iters){
  int i = blockIdx.x*blockDim.x + threadIdx.x;
  if(i<n){ float v=a[i]; for(int k=0;k<iters;k++){ v=v*1.0000001f+0.0001f; v=v*0.9999999f; } a[i]=v; }
}

int main(int argc, char** argv){
  const char* mode = argc>1? argv[1] : "sustained";
  int secs = argc>2? atoi(argv[2]) : 20;
  double mem_gb = argc>3? atof(argv[3]) : 2.0;
  signal(SIGTERM,on_sig); signal(SIGINT,on_sig);

  int dev=0; hipGetDevice(&dev);
  printf("WORKLOAD pid=%d dev=%d mode=%s secs=%d mem_gb=%.2f\n", getpid(), dev, mode, secs, mem_gb);
  fflush(stdout);

  size_t bytes = (size_t)(mem_gb*1024.0*1024.0*1024.0);
  size_t n = bytes/sizeof(float);
  float* d=nullptr;
  hipError_t e = hipMalloc(&d, bytes);
  if(e!=hipSuccess){ printf("WORKLOAD alloc-failed %s\n", hipGetErrorString(e)); fflush(stdout); return 3; }
  hipMemset(d, 0, bytes);
  printf("WORKLOAD allocated_gb=%.2f\n", mem_gb); fflush(stdout);

  int blocks=(n+255)/256; if(blocks>65535) blocks=65535;
  time_t start=time(nullptr);
  long it=0;
  while(!stop_flag && (time(nullptr)-start) < secs){
    if(strcmp(mode,"idle")==0){ usleep(200000); }
    else if(strcmp(mode,"memramp")==0){ usleep(100000); }
    else {
      hipLaunchKernelGGL(burn, dim3(blocks), dim3(256), 0, 0, d, (int)((n>1u<<28)?(1u<<28):n), 200);
      hipDeviceSynchronize();
    }
    if((++it)%5==0){ printf("WORKLOAD heartbeat it=%ld t=%ld\n", it, (long)(time(nullptr)-start)); fflush(stdout); }
  }
  hipFree(d);
  printf("WORKLOAD exit pid=%d reason=%s\n", getpid(), stop_flag?"signal":"duration"); fflush(stdout);
  return 0;
}
