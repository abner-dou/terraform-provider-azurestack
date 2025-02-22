package network

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/profiles/2020-09-01/network/mgmt/network"
	"github.com/hashicorp/go-azure-helpers/lang/pointer"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/hashicorp/terraform-provider-azurestack/internal/clients"
	"github.com/hashicorp/terraform-provider-azurestack/internal/locks"
	lbparse "github.com/hashicorp/terraform-provider-azurestack/internal/services/loadbalancer/parse"
	loadbalancer "github.com/hashicorp/terraform-provider-azurestack/internal/services/loadbalancer/validate"
	"github.com/hashicorp/terraform-provider-azurestack/internal/services/network/parse"
	"github.com/hashicorp/terraform-provider-azurestack/internal/services/network/validate"
	"github.com/hashicorp/terraform-provider-azurestack/internal/tf"
	"github.com/hashicorp/terraform-provider-azurestack/internal/tf/pluginsdk"
	"github.com/hashicorp/terraform-provider-azurestack/internal/tf/timeouts"
	"github.com/hashicorp/terraform-provider-azurestack/internal/utils"
)

func loadBalancerBackendAddressPoolAssociation() *pluginsdk.Resource {
	return &pluginsdk.Resource{
		Create: loadBalancerBackendAddressPoolAssociationCreateUpdate,
		Read:   loadBalancerBackendAddressPoolAssociationRead,
		Delete: loadBalancerBackendAddressPoolAssociationDelete,

		Importer: pluginsdk.ImporterValidatingResourceId(func(id string) error {
			splitId := strings.Split(id, "|")
			if _, err := parse.NetworkInterfaceIpConfigurationID(splitId[0]); err != nil {
				return err
			}
			if _, err := lbparse.LoadBalancerBackendAddressPoolID(splitId[1]); err != nil {
				return err
			}
			return nil
		}),

		Timeouts: &pluginsdk.ResourceTimeout{
			Create: pluginsdk.DefaultTimeout(30 * time.Minute),
			Read:   pluginsdk.DefaultTimeout(5 * time.Minute),
			Update: pluginsdk.DefaultTimeout(30 * time.Minute),
			Delete: pluginsdk.DefaultTimeout(30 * time.Minute),
		},

		Schema: map[string]*pluginsdk.Schema{
			"network_interface_id": {
				Type:         pluginsdk.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validate.NetworkInterfaceID,
			},

			"ip_configuration_name": {
				Type:         pluginsdk.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringIsNotEmpty,
			},

			"backend_address_pool_id": {
				Type:         pluginsdk.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: loadbalancer.LoadBalancerBackendAddressPoolID,
			},
		},
	}
}

func loadBalancerBackendAddressPoolAssociationCreateUpdate(d *pluginsdk.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Network.InterfacesClient
	ctx, cancel := timeouts.ForCreate(meta.(*clients.Client).StopContext, d)
	defer cancel()

	log.Printf("[INFO] preparing arguments for Network Interface <-> Load Balancer Backend Address Pool Association creation.")

	networkInterfaceId := d.Get("network_interface_id").(string)
	ipConfigurationName := d.Get("ip_configuration_name").(string)
	backendAddressPoolId := d.Get("backend_address_pool_id").(string)

	id, err := parse.NetworkInterfaceID(networkInterfaceId)
	if err != nil {
		return err
	}

	locks.ByName(id.Name, networkInterfaceResourceName)
	defer locks.UnlockByName(id.Name, networkInterfaceResourceName)

	read, err := client.Get(ctx, id.ResourceGroup, id.Name, "")
	if err != nil {
		if utils.ResponseWasNotFound(read.Response) {
			return fmt.Errorf("%s was not found!", *id)
		}

		return fmt.Errorf("retrieving %s: %+v", *id, err)
	}

	props := read.InterfacePropertiesFormat
	if props == nil {
		return fmt.Errorf("Error: `properties` was nil for %s", *id)
	}

	ipConfigs := props.IPConfigurations
	if ipConfigs == nil {
		return fmt.Errorf("Error: `properties.IPConfigurations` was nil for %s", *id)
	}

	c := FindNetworkInterfaceIPConfiguration(props.IPConfigurations, ipConfigurationName)
	if c == nil {
		return fmt.Errorf("Error: IP Configuration %q was not found on %s", ipConfigurationName, *id)
	}

	config := *c
	p := config.InterfaceIPConfigurationPropertiesFormat
	if p == nil {
		return fmt.Errorf("Error: `IPConfiguration.properties` was nil for %s", *id)
	}

	pools := make([]network.BackendAddressPool, 0)

	// first double-check it doesn't exist
	resourceId := fmt.Sprintf("%s/ipConfigurations/%s|%s", networkInterfaceId, ipConfigurationName, backendAddressPoolId)
	if p.LoadBalancerBackendAddressPools != nil {
		for _, existingPool := range *p.LoadBalancerBackendAddressPools {
			if id := existingPool.ID; id != nil {
				if *id == backendAddressPoolId {
					return tf.ImportAsExistsError("azurerm_network_interface_backend_address_pool_association", resourceId)
				}

				pools = append(pools, existingPool)
			}
		}
	}

	pool := network.BackendAddressPool{
		ID: pointer.FromString(backendAddressPoolId),
	}
	pools = append(pools, pool)
	p.LoadBalancerBackendAddressPools = &pools

	props.IPConfigurations = updateNetworkInterfaceIPConfiguration(config, props.IPConfigurations)

	future, err := client.CreateOrUpdate(ctx, id.ResourceGroup, id.Name, read)
	if err != nil {
		return fmt.Errorf("updating Backend Address Pool Association for %s: %+v", *id, err)
	}

	if err = future.WaitForCompletionRef(ctx, client.Client); err != nil {
		return fmt.Errorf("waiting for completion of Backend Address Pool Association for %s: %+v", *id, err)
	}

	d.SetId(resourceId)

	return loadBalancerBackendAddressPoolAssociationRead(d, meta)
}

func loadBalancerBackendAddressPoolAssociationRead(d *pluginsdk.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Network.InterfacesClient
	ctx, cancel := timeouts.ForRead(meta.(*clients.Client).StopContext, d)
	defer cancel()

	splitId := strings.Split(d.Id(), "|")
	if len(splitId) != 2 {
		return fmt.Errorf("Expected ID to be in the format {networkInterfaceId}/ipConfigurations/{ipConfigurationName}|{backendAddressPoolId} but got %q", d.Id())
	}

	nicID, err := parse.NetworkInterfaceIpConfigurationID(splitId[0])
	if err != nil {
		return err
	}

	backendAddressPoolId := splitId[1]

	read, err := client.Get(ctx, nicID.ResourceGroup, nicID.NetworkInterfaceName, "")
	if err != nil {
		if utils.ResponseWasNotFound(read.Response) {
			log.Printf("Network Interface %q (Resource Group %q) was not found - removing from state!", nicID.NetworkInterfaceName, nicID.ResourceGroup)
			d.SetId("")
			return nil
		}

		return fmt.Errorf("retrieving Network Interface %q (Resource Group %q): %+v", nicID.NetworkInterfaceName, nicID.ResourceGroup, err)
	}

	nicProps := read.InterfacePropertiesFormat
	if nicProps == nil {
		return fmt.Errorf("Error: `properties` was nil for Network Interface %q (Resource Group %q)", nicID.NetworkInterfaceName, nicID.ResourceGroup)
	}

	ipConfigs := nicProps.IPConfigurations
	if ipConfigs == nil {
		return fmt.Errorf("Error: `properties.IPConfigurations` was nil for Network Interface %q (Resource Group %q)", nicID.NetworkInterfaceName, nicID.ResourceGroup)
	}

	c := FindNetworkInterfaceIPConfiguration(nicProps.IPConfigurations, nicID.IpConfigurationName)
	if c == nil {
		log.Printf("IP Configuration %q was not found in Network Interface %q (Resource Group %q) - removing from state!", nicID.IpConfigurationName, nicID.NetworkInterfaceName, nicID.ResourceGroup)
		d.SetId("")
		return nil
	}
	config := *c

	found := false
	if props := config.InterfaceIPConfigurationPropertiesFormat; props != nil {
		if backendPools := props.LoadBalancerBackendAddressPools; backendPools != nil {
			for _, pool := range *backendPools {
				if pool.ID == nil {
					continue
				}

				if *pool.ID == backendAddressPoolId {
					found = true
					break
				}
			}
		}
	}

	if !found {
		log.Printf("[DEBUG] Association between Network Interface %q (Resource Group %q) and Load Balancer Backend Pool %q was not found - removing from state!", nicID.NetworkInterfaceName, nicID.ResourceGroup, backendAddressPoolId)
		d.SetId("")
		return nil
	}

	d.Set("backend_address_pool_id", backendAddressPoolId)
	d.Set("ip_configuration_name", nicID.IpConfigurationName)
	d.Set("network_interface_id", read.ID)

	return nil
}

func loadBalancerBackendAddressPoolAssociationDelete(d *pluginsdk.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Network.InterfacesClient
	ctx, cancel := timeouts.ForDelete(meta.(*clients.Client).StopContext, d)
	defer cancel()

	splitId := strings.Split(d.Id(), "|")
	if len(splitId) != 2 {
		return fmt.Errorf("Expected ID to be in the format {networkInterfaceId}/ipConfigurations/{ipConfigurationName}|{backendAddressPoolId} but got %q", d.Id())
	}

	nicID, err := parse.NetworkInterfaceIpConfigurationID(splitId[0])
	if err != nil {
		return err
	}

	backendAddressPoolId := splitId[1]

	locks.ByName(nicID.NetworkInterfaceName, networkInterfaceResourceName)
	defer locks.UnlockByName(nicID.NetworkInterfaceName, networkInterfaceResourceName)

	read, err := client.Get(ctx, nicID.ResourceGroup, nicID.NetworkInterfaceName, "")
	if err != nil {
		if utils.ResponseWasNotFound(read.Response) {
			return fmt.Errorf("Network Interface %q (Resource Group %q) was not found!", nicID.NetworkInterfaceName, nicID.ResourceGroup)
		}

		return fmt.Errorf("retrieving Network Interface %q (Resource Group %q): %+v", nicID.NetworkInterfaceName, nicID.ResourceGroup, err)
	}

	nicProps := read.InterfacePropertiesFormat
	if nicProps == nil {
		return fmt.Errorf("Error: `properties` was nil for Network Interface %q (Resource Group %q)", nicID.NetworkInterfaceName, nicID.ResourceGroup)
	}

	ipConfigs := nicProps.IPConfigurations
	if ipConfigs == nil {
		return fmt.Errorf("Error: `properties.IPConfigurations` was nil for Network Interface %q (Resource Group %q)", nicID.NetworkInterfaceName, nicID.ResourceGroup)
	}

	c := FindNetworkInterfaceIPConfiguration(nicProps.IPConfigurations, nicID.IpConfigurationName)
	if c == nil {
		return fmt.Errorf("Error: IP Configuration %q was not found on Network Interface %q (Resource Group %q)", nicID.IpConfigurationName, nicID.NetworkInterfaceName, nicID.ResourceGroup)
	}
	config := *c

	props := config.InterfaceIPConfigurationPropertiesFormat
	if props == nil {
		return fmt.Errorf("Error: Properties for IPConfiguration %q was nil for Network Interface %q (Resource Group %q)", nicID.IpConfigurationName, nicID.NetworkInterfaceName, nicID.ResourceGroup)
	}

	backendAddressPools := make([]network.BackendAddressPool, 0)
	if backendPools := props.LoadBalancerBackendAddressPools; backendPools != nil {
		for _, pool := range *backendPools {
			if pool.ID == nil {
				continue
			}

			if *pool.ID != backendAddressPoolId {
				backendAddressPools = append(backendAddressPools, pool)
			}
		}
	}
	props.LoadBalancerBackendAddressPools = &backendAddressPools
	nicProps.IPConfigurations = updateNetworkInterfaceIPConfiguration(config, nicProps.IPConfigurations)

	future, err := client.CreateOrUpdate(ctx, nicID.ResourceGroup, nicID.NetworkInterfaceName, read)
	if err != nil {
		return fmt.Errorf("removing Backend Address Pool Association for Network Interface %q (Resource Group %q): %+v", nicID.NetworkInterfaceName, nicID.ResourceGroup, err)
	}

	if err = future.WaitForCompletionRef(ctx, client.Client); err != nil {
		return fmt.Errorf("waiting for removal of Backend Address Pool Association for NIC %q (Resource Group %q): %+v", nicID.NetworkInterfaceName, nicID.ResourceGroup, err)
	}

	return nil
}
