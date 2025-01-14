// Copyright 2017 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#include "agones/sdk.h"

#include <grpcpp/grpcpp.h>
#include <utility>

namespace agones {

struct SDK::SDKImpl {
  std::shared_ptr<grpc::Channel> channel_;
  std::unique_ptr<stable::agones::dev::sdk::SDK::Stub> stub_;
  std::unique_ptr<grpc::ClientWriter<stable::agones::dev::sdk::Empty>> health_;
};

SDK::SDK() : pimpl_{std::make_unique<SDKImpl>()} {
  pimpl_->channel_ = grpc::CreateChannel("localhost:59357",
                                         grpc::InsecureChannelCredentials());
}

SDK::~SDK() {}

bool SDK::Connect() {
  if (!pimpl_->channel_->WaitForConnected(
          gpr_time_add(gpr_now(GPR_CLOCK_REALTIME),
                       gpr_time_from_seconds(30, GPR_TIMESPAN)))) {
    return false;
  }

  pimpl_->stub_ = stable::agones::dev::sdk::SDK::NewStub(pimpl_->channel_);

  // make the health connection
  stable::agones::dev::sdk::Empty response;
  grpc::ClientContext context;
  pimpl_->health_ = pimpl_->stub_->Health(&context, &response);

  return true;
}

grpc::Status SDK::Ready() {
  grpc::ClientContext context;
  context.set_deadline(gpr_time_add(gpr_now(GPR_CLOCK_REALTIME),
                                    gpr_time_from_seconds(30, GPR_TIMESPAN)));
  stable::agones::dev::sdk::Empty request;
  stable::agones::dev::sdk::Empty response;

  return pimpl_->stub_->Ready(&context, request, &response);
}

bool SDK::Health() {
  stable::agones::dev::sdk::Empty request;
  return pimpl_->health_->Write(request);
}

grpc::Status SDK::GameServer(stable::agones::dev::sdk::GameServer* response) {
  grpc::ClientContext context;
  context.set_deadline(gpr_time_add(gpr_now(GPR_CLOCK_REALTIME),
                                    gpr_time_from_seconds(30, GPR_TIMESPAN)));
  stable::agones::dev::sdk::Empty request;

  return pimpl_->stub_->GetGameServer(&context, request, response);
}

grpc::Status SDK::WatchGameServer(
    const std::function<void(stable::agones::dev::sdk::GameServer)>& callback) {
  grpc::ClientContext context;
  stable::agones::dev::sdk::Empty request;
  stable::agones::dev::sdk::GameServer gameServer;

  std::unique_ptr<grpc::ClientReader<stable::agones::dev::sdk::GameServer>>
      reader = pimpl_->stub_->WatchGameServer(&context, request);
  while (reader->Read(&gameServer)) {
    callback(gameServer);
  }
  return reader->Finish();
}

grpc::Status SDK::Shutdown() {
  grpc::ClientContext context;
  context.set_deadline(gpr_time_add(gpr_now(GPR_CLOCK_REALTIME),
                                    gpr_time_from_seconds(30, GPR_TIMESPAN)));
  stable::agones::dev::sdk::Empty request;
  stable::agones::dev::sdk::Empty response;

  return pimpl_->stub_->Shutdown(&context, request, &response);
}

grpc::Status SDK::SetLabel(std::string key, std::string value) {
  grpc::ClientContext context;
  context.set_deadline(gpr_time_add(gpr_now(GPR_CLOCK_REALTIME),
                                    gpr_time_from_seconds(30, GPR_TIMESPAN)));

  stable::agones::dev::sdk::KeyValue request;
  request.set_key(std::move(key));
  request.set_value(std::move(value));

  stable::agones::dev::sdk::Empty response;

  return pimpl_->stub_->SetLabel(&context, request, &response);
}

grpc::Status SDK::SetAnnotation(std::string key, std::string value) {
  grpc::ClientContext context;
  context.set_deadline(gpr_time_add(gpr_now(GPR_CLOCK_REALTIME),
                                    gpr_time_from_seconds(30, GPR_TIMESPAN)));

  stable::agones::dev::sdk::KeyValue request;
  request.set_key(std::move(key));
  request.set_value(std::move(value));

  stable::agones::dev::sdk::Empty response;

  return pimpl_->stub_->SetAnnotation(&context, request, &response);
}
}  // namespace agones
