//! A deterministic virtual CDC ACM USB/IP device for the Kata product E2E.

use log::info;
use std::any::Any;
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::sync::{Arc, Mutex};
use usbip::{ClassCode, Direction, EndpointAttributes, SetupPacket, UsbEndpoint};

#[derive(Debug, Default)]
struct EchoHandler {
    pending: Vec<u8>,
}

impl EchoHandler {
    fn endpoints() -> Vec<UsbEndpoint> {
        vec![
            UsbEndpoint {
                address: 0x81,
                attributes: EndpointAttributes::Interrupt as u8,
                max_packet_size: 8,
                interval: 10,
            },
            UsbEndpoint {
                address: 0x82,
                attributes: EndpointAttributes::Bulk as u8,
                max_packet_size: 512,
                interval: 0,
            },
            UsbEndpoint {
                address: 0x02,
                attributes: EndpointAttributes::Bulk as u8,
                max_packet_size: 512,
                interval: 0,
            },
        ]
    }
}

impl usbip::UsbInterfaceHandler for EchoHandler {
    fn get_class_specific_descriptor(&self) -> Vec<u8> {
        vec![0x05, 0x24, 0x00, 0x10, 0x01, 0x04, 0x24, 0x02, 0x00]
    }

    fn handle_urb(
        &mut self,
        _interface: &usbip::UsbInterface,
        endpoint: UsbEndpoint,
        transfer_buffer_length: u32,
        _setup: SetupPacket,
        request: &[u8],
    ) -> std::io::Result<Vec<u8>> {
        if endpoint.attributes != EndpointAttributes::Bulk as u8 {
            return Ok(Vec::new());
        }
        match endpoint.direction() {
            Direction::Out => {
                self.pending.extend_from_slice(request);
                Ok(Vec::new())
            }
            Direction::In => {
                let length = self.pending.len().min(transfer_buffer_length as usize);
                Ok(self.pending.drain(..length).collect())
            }
        }
    }

    fn as_any(&mut self) -> &mut dyn Any {
        self
    }
}

fn device() -> usbip::UsbDevice {
    let handler: Arc<Mutex<Box<dyn usbip::UsbInterfaceHandler + Send>>> =
        Arc::new(Mutex::new(Box::new(EchoHandler::default())));
    let mut device = usbip::UsbDevice::new(1).with_interface(
        ClassCode::CDC as u8,
        0x02,
        0,
        Some("Codespace USB/IP CDC echo"),
        EchoHandler::endpoints(),
        handler,
    );
    device.bus_id = "1-1".to_string();
    device.usb_version = 0x0200u16.into();
    device
}

#[tokio::main]
async fn main() {
    env_logger::init();
    let first = SocketAddr::new(IpAddr::V4(Ipv4Addr::LOCALHOST), 3240);
    let second = SocketAddr::new(IpAddr::V4(Ipv4Addr::LOCALHOST), 3241);
    info!("serving independent virtual CDC ACM devices with bus ID 1-1 on {first} and {second}");
    tokio::join!(
        usbip::server(
            first,
            Arc::new(usbip::UsbIpServer::new_simulated(vec![device()]))
        ),
        usbip::server(
            second,
            Arc::new(usbip::UsbIpServer::new_simulated(vec![device()]))
        )
    );
}
