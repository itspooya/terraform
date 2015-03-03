package aws

import (
	"bytes"
	//"crypto/sha1"
	//"encoding/hex"
	"fmt"
	"log"
	//"strconv"
	//"strings"
	//"time"

	"github.com/hashicorp/aws-sdk-go/aws"
	"github.com/hashicorp/terraform/helper/hashcode"
	//"github.com/hashicorp/terraform/helper/resource"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/aws-sdk-go/gen/ec2"
)

func resourceAwsNetworkInterface() *schema.Resource {
	return &schema.Resource{
		Create: resourceAwsNetworkInterfaceCreate,
		Read:   resourceAwsNetworkInterfaceRead,
		Update: resourceAwsNetworkInterfaceUpdate,
		Delete: resourceAwsNetworkInterfaceDelete,

		Schema: map[string]*schema.Schema{			
			
			"subnet_id": &schema.Schema{
				Type:     schema.TypeString,
				Required: true,				
				ForceNew: true,
			},

			"private_ips": &schema.Schema{
				Type:     schema.TypeSet,
				Optional: true,
				ForceNew: true,				
				Elem:     &schema.Schema{Type: schema.TypeString},
				Set: func(v interface{}) int {
					return hashcode.String(v.(string))
				},
			},

			"security_groups": &schema.Schema{
				Type:     schema.TypeSet,
				Optional: true,
				Computed: true,				
				Elem:     &schema.Schema{Type: schema.TypeString},
				Set: func(v interface{}) int {
					return hashcode.String(v.(string))
				},
			},

			"attachment": &schema.Schema{
				Type:     schema.TypeSet,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"instance": &schema.Schema{
							Type:     schema.TypeString,
							Required: true,
						},
						"device_index": &schema.Schema{
							Type:     schema.TypeInt,
							Required: true,
						},
						"attachment_id": &schema.Schema{
							Type:     schema.TypeString,							
						},
					},
				},
				Set: resourceAwsEniAttachmentHash,
			},

			"tags": tagsSchema(),			
		},
	}
}

func resourceAwsNetworkInterfaceCreate(d *schema.ResourceData, meta interface{}) error {
	
	ec2conn := meta.(*AWSClient).ec2conn2

	request := &ec2.CreateNetworkInterfaceRequest{		
		Groups: 				expandStringList(d.Get("security_groups").(*schema.Set).List()),
		SubnetID:				aws.String(d.Get("subnet_id").(string)),
		PrivateIPAddresses:		convertToPrivateIPAddresses(d.Get("private_ips").(*schema.Set).List()),
	}
	
	log.Printf("[DEBUG] Creating network interface")
	resp, err := ec2conn.CreateNetworkInterface(request)
	if err != nil {
		return fmt.Errorf("Error creating ENI: %s", err)
	}

	new_interface_id := *resp.NetworkInterface.NetworkInterfaceID
	d.SetId(new_interface_id)
	log.Printf("[INFO] ENI ID: %s", d.Id())

	return resourceAwsNetworkInterfaceUpdate(d, meta)	
}

func resourceAwsNetworkInterfaceRead(d *schema.ResourceData, meta interface{}) error {

	ec2conn := meta.(*AWSClient).ec2conn2
	describe_network_interfaces_request := &ec2.DescribeNetworkInterfacesRequest{		
		NetworkInterfaceIDs:	[]string{d.Id()},
	}
	describeResp, err := ec2conn.DescribeNetworkInterfaces(describe_network_interfaces_request)

	if err != nil {
		if ec2err, ok := err.(aws.APIError); ok && ec2err.Code == "InvalidNetworkInterfaceID.NotFound" {
			// The ENI is gone now, so just remove it from the state
			d.SetId("")
			return nil
		}
		
		return fmt.Errorf("Error retrieving ENI: %s", err)
	}
	if len(describeResp.NetworkInterfaces) != 1 {
		return fmt.Errorf("Unable to find ENI: %#v", describeResp.NetworkInterfaces)
	}

	eni := describeResp.NetworkInterfaces[0]
	d.Set("subnet_id", eni.SubnetID)
	d.Set("private_ips", convertToJustAddresses(eni.PrivateIPAddresses))
	d.Set("security_groups", convertToGroupIds(eni.Groups))

	if eni.Attachment != nil {
		d.Set("attachment", flattenAttachment(eni.Attachment))
	} else {
		d.Set("attachment", nil)
	}
	
	return nil
}

func resourceAwsNetworkInterfaceUpdate(d *schema.ResourceData, meta interface{}) error {

	d.Partial(true)

	if d.HasChange("attachment") {
		ec2conn := meta.(*AWSClient).ec2conn2
		oa, na := d.GetChange("attachment")			
		
		// if there was an old attachment, remove it
		if oa != nil && len(oa.(*schema.Set).List()) > 0 {			
			old_attachment := oa.(*schema.Set).List()[0].(map[string]interface{})
			detach_request := &ec2.DetachNetworkInterfaceRequest{
				AttachmentID: 	aws.String(old_attachment["attachment_id"].(string)),
				Force:			aws.Boolean(true),
			}
			detach_err := ec2conn.DetachNetworkInterface(detach_request)
			if detach_err != nil {
				return fmt.Errorf("Error detaching ENI: %s", detach_err)
			}
		}

		// if there is a new attachment, attach it
		if na != nil && len(na.(*schema.Set).List()) > 0 {				
			new_attachment := na.(*schema.Set).List()[0].(map[string]interface{})
			attach_request := &ec2.AttachNetworkInterfaceRequest{
				DeviceIndex:		aws.Integer(new_attachment["device_index"].(int)),
				InstanceID:			aws.String(new_attachment["instance"].(string)),
				NetworkInterfaceID:	aws.String(d.Id()),
			}
			_, attach_err := ec2conn.AttachNetworkInterface(attach_request)
			if attach_err != nil {
				return fmt.Errorf("Error attaching ENI: %s", attach_err)
			}
		}

		d.SetPartial("attachment")
	}

	if d.HasChange("security_groups") {
		request := &ec2.ModifyNetworkInterfaceAttributeRequest{
			NetworkInterfaceID:		aws.String(d.Id()),
			Groups:					expandStringList(d.Get("security_groups").(*schema.Set).List()),
		}

		ec2conn := meta.(*AWSClient).ec2conn2
		err := ec2conn.ModifyNetworkInterfaceAttribute(request)
		if err != nil {
			return fmt.Errorf("Failure updating ENI: %s", err)
		}

		d.SetPartial("security_groups")
	}

	d.Partial(false)

	return resourceAwsNetworkInterfaceRead(d, meta)
}

func resourceAwsNetworkInterfaceDelete(d *schema.ResourceData, meta interface{}) error {
	ec2conn := meta.(*AWSClient).ec2conn2

	log.Printf("[INFO] Deleting ENI: %s", d.Id())

	deleteEniOpts := ec2.DeleteNetworkInterfaceRequest{
		NetworkInterfaceID: aws.String(d.Id()),
	}
	if err := ec2conn.DeleteNetworkInterface(&deleteEniOpts); err != nil {
		return fmt.Errorf("Error deleting ENI: %s", err)
	}

	return nil
}

func convertToJustAddresses(dtos []ec2.NetworkInterfacePrivateIPAddress) []string {
	ips := make([]string, 0, len(dtos))
	for _, v := range dtos {
		ip := *v.PrivateIPAddress
		ips = append(ips, ip)
	}
	return ips
}

func convertToGroupIds(dtos []ec2.GroupIdentifier) []string {
	ids := make([]string, 0, len(dtos))
	for _, v := range dtos {
		group_id := *v.GroupID
		ids = append(ids, group_id)
	}
	return ids
}

func convertToPrivateIPAddresses(ips []interface{}) []ec2.PrivateIPAddressSpecification {
	dtos := make([]ec2.PrivateIPAddressSpecification, 0, len(ips))
	for i, v := range ips {
		new_private_ip := ec2.PrivateIPAddressSpecification{
			PrivateIPAddress:	aws.String(v.(string)),
		}	
		
		if i == 0 {
			new_private_ip.Primary = aws.Boolean(true)
		}

		dtos = append(dtos, new_private_ip)
	}
	return dtos
}

func resourceAwsEniAttachmentHash(v interface{}) int {
	var buf bytes.Buffer
	m := v.(map[string]interface{})
	buf.WriteString(fmt.Sprintf("%d-", m["instance"].(string)))
	buf.WriteString(fmt.Sprintf("%s-", m["device_index"].(int)))
	return hashcode.String(buf.String())
}

func flattenAttachment(a *ec2.NetworkInterfaceAttachment) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, 1)
	att := make(map[string]interface{})	
	att["instance"] = *a.InstanceID
	att["device_index"] = *a.DeviceIndex
	att["attachment_id"] = *a.AttachmentID
	result = append(result, att)
	return result
}